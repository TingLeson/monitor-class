/**
 * 学生端 LiveKit 适配层（§20 / §26 / §28 / §44）。
 *
 * 整个 app 里**只有这个文件**import `livekit-client`，而且是被
 * `media-room.ts` 的默认工厂**动态**加载的（见那边的说明）。
 *
 * 三条从这里流出去的硬约束：
 *
 * 1. `connect(url, token, { autoSubscribe: false })`（§26/§28）。学生端永远不主动
 *    订阅任何人的轨道；即便服务端已用 Subscription Permission 挡住其他学生，
 *    自动订阅也不该有"先拉下来再过滤"的机会。老师私密语音是**唯一**的例外，
 *    而它的订阅权在服务端（§31 的 `UpdateSubscriptions`）：本文件只负责把被推下来的
 *    那条麦克风轨道播出来（`TrackSubscribed` → `attach()`）。
 * 2. 本地轨道必须复用**已经拿到授权的那一条** `MediaStreamTrack`：屏幕那条来自
 *    Phase 5 的 Gate（§20），摄像头那条来自学生点击后的 `getUserMedia`（§24），
 *    麦克风那条同样来自学生点击（§25）。这里只接受现成的 track，绝不调用
 *    `createLocalScreenTracks()` / `getDisplayMedia()` / `createLocalVideoTrack()` /
 *    `createLocalAudioTrack()` 之类的"重新采集"API——那会让学生看到第二次授权弹窗
 *    或一个他没点过的设备授权。
 * 3. 地址与 token 只在这里被交给 SDK，不缓存、不打印（§44）。本文件没有
 *    `console.*`，异常也原样上抛，由 store 折叠成中文提示。
 */

import {
  ConnectionQuality,
  DisconnectReason,
  LocalAudioTrack,
  LocalVideoTrack,
  Room,
  RoomEvent,
  Track,
  type Participant,
  type RemoteAudioTrack,
  type RemoteTrack,
  type RemoteTrackPublication,
} from 'livekit-client'
import type {
  ConnectionQualityLevel,
  MediaCredentials,
  MediaDisconnectReason,
  ScreenPublisherRoom,
  TeacherAudioState,
} from './media-room.ts'

/**
 * SDK 的 `ConnectionQuality` 枚举 → 我们的等级类型。
 *
 * 两者字符串逐字相同，因此是纯透传；写成显式 switch 是为了让 SDK 未来新增枚举值时
 * 这里会落到 `default`，而不是把一个我们没想过的值当成合法等级送进界面。
 * `Lost` 尤其重要：它是"连不上了"，必须与 `Poor`（还连着但很差）区分开（§52）。
 */
function toQualityLevel(quality: ConnectionQuality): ConnectionQualityLevel {
  switch (quality) {
    case ConnectionQuality.Excellent:
      return 'excellent'
    case ConnectionQuality.Good:
      return 'good'
    case ConnectionQuality.Poor:
      return 'poor'
    case ConnectionQuality.Lost:
      return 'lost'
    default:
      return 'unknown'
  }
}

/** 断开原因：SDK 用数字枚举，这里换成可读名字，便于排障时看出"是谁断的"。 */
function describeDisconnectReason(reason: DisconnectReason | undefined): MediaDisconnectReason {
  if (reason === undefined) return null
  return DisconnectReason[reason] ?? String(reason)
}

export function createLiveKitScreenPublisherRoom(
  credentials: MediaCredentials,
): ScreenPublisherRoom {
  const room = new Room({
    /**
     * 屏幕共享只有一个视频层，自适应流按 `<video>` 元素的实际尺寸选层、
     * 元素不可见时暂停下行；dynacast 让没有被任何人消费的层停止上行。
     * 两者都是 LiveKit 的既有能力，符合 §52「不要在 V1 自己实现 RTP ABR」。
     *
     * 注意：学生端自己的页面上**没有** `<video>`（§56），所以自适应流对
     * 学生端几乎不起作用；开在这里是为了让"发布端"与"订阅端"的行为一致，
     * 也避免将来 Phase 9 引入摄像头预览时忘了开。
     */
    adaptiveStream: true,
    dynacast: true,
    /**
     * unpublish 时不替我们停掉本地轨道。
     *
     * WHY：这条轨道是 Phase 5 的 Gate 产物，所有权在捕获层（`screen-capture.ts`）。
     * 若 SDK 在 unpublish 时顺手 `stop()`，§22 的"重新共享"就必须重新授权一次——
     * 而"停止共享"（例如老师关课堂）与"轨道丢失"是两种不同路径，
     * 前者我们才需要真正释放，后者轨道早就死了。让释放点只有一个。
     */
    stopLocalTrackOnUnpublish: false,
    /**
     * 页面关闭/刷新时 SDK 自己尽力断开（我们另有 beforeunload 的同步兜底）。
     * 显式写出来是因为它关系到"学生关掉标签页后老师那端多久变红"，
     * 不能依赖 SDK 的默认值悄悄变化。
     */
    disconnectOnPageLeave: true,
  })

  /** 当前已发布的本地屏幕轨道；重新发布时会被替换。 */
  let publishedTrack: LocalVideoTrack | null = null
  /**
   * 当前已发布的本地摄像头轨道（§24）。
   *
   * 与屏幕那条**分开存**：两条轨道独立开关（学生可以只开摄像头、也可以在共享屏幕的
   * 同时开摄像头），共用一个变量会让"关闭摄像头"把屏幕一起撤下来。
   */
  let publishedCameraTrack: LocalVideoTrack | null = null
  /**
   * 当前已发布的本地麦克风轨道（§25）。
   *
   * 第三条独立变量，理由与摄像头完全相同：屏幕 / 摄像头 / 麦克风是三条互不相干的
   * 轨道，"关麦"绝不能顺手把画面撤下来。
   */
  let publishedMicrophoneTrack: LocalAudioTrack | null = null

  /* ---------------------------------------------------------------------- */
  /* 老师私密语音的音频播放（§31 / §32）                                      */
  /* ---------------------------------------------------------------------- */

  /**
   * 老师麦克风的轨道与它当前挂着的 `<audio>`。
   *
   * WHY 由适配层自己在房间事件上挂载，而不是等 store 来 attach：
   * 这条轨道的**订阅权在服务端**（§31 的 `UpdateSubscriptions`），它随时可能
   * 被推下来；而 `autoSubscribe = false`（§26/§28）意味着 SDK **不会**自动播放
   * 任何远端音频。少挂这一次，学生就会在"正在与老师语音沟通"的提示下什么都听不到。
   */
  let teacherAudioTrack: RemoteAudioTrack | null = null
  let teacherAudioElement: HTMLMediaElement | null = null
  let audioState: TeacherAudioState = 'idle'
  const audioListeners = new Set<(state: TeacherAudioState) => void>()

  /**
   * 重算并广播音频状态。
   *
   * `canPlaybackAudio` 是 SDK 对"自动播放策略是否放行"的权威回答：轨道在、
   * 但浏览器拦住了播放时，界面必须能说出"点一下才能听到"（见 TeacherAudioState）。
   */
  function refreshAudioState(): void {
    const next: TeacherAudioState =
      teacherAudioTrack === null ? 'idle' : room.canPlaybackAudio ? 'playing' : 'blocked'
    if (next === audioState) return
    audioState = next
    for (const listener of [...audioListeners]) listener(next)
  }

  /** 摘掉老师音频（取消订阅 / 掉线 / 断开）：元素必须被移除，否则会留下一个静音黑洞。 */
  function detachTeacherAudio(): void {
    if (teacherAudioTrack !== null && teacherAudioElement !== null) {
      teacherAudioTrack.detach(teacherAudioElement)
    }
    teacherAudioTrack = null
    teacherAudioElement = null
    refreshAudioState()
  }

  /** 服务端把老师麦克风推下来了：播放它（声音是这条轨道存在的唯一意义）。 */
  function attachTeacherAudio(track: RemoteAudioTrack): void {
    detachTeacherAudio()
    teacherAudioTrack = track
    // 不传元素：SDK 会创建一个 `<audio>` 元素并开始播放（真实 Chrome 需要它才出声）。
    teacherAudioElement = track.attach()
    /**
     * `startAudio()` 是 SDK 提供的"在用户手势之后恢复播放"的入口。这里顺手调一次：
     * 学生进入课堂时已经点过按钮（页面有 sticky activation），因此它通常直接成功；
     * 失败了也不抛到界面——`canPlaybackAudio` 会把状态标成 blocked，
     * 由学生点"开启声音"重试（那时一定在手势里）。
     */
    void room
      .startAudio()
      .catch(() => undefined)
      .then(refreshAudioState)
    refreshAudioState()
  }

  room
    .on(RoomEvent.TrackSubscribed, (track: RemoteTrack, publication: RemoteTrackPublication) => {
      /**
       * 只认 `microphone` 源，而且只有老师会发布它（§27：老师 Token 唯一允许的
       * 发布源就是 microphone）。学生之间的音频在服务端就被撤销了订阅（§26/§32），
       * 即便漏过来，这里也不会把它当成"老师"播出来。
       */
      if (publication.source !== Track.Source.Microphone) return
      if (track.kind !== Track.Kind.Audio) return
      attachTeacherAudio(track as RemoteAudioTrack)
    })
    .on(RoomEvent.TrackUnsubscribed, (_track: RemoteTrack, publication: RemoteTrackPublication) => {
      if (publication.source !== Track.Source.Microphone) return
      detachTeacherAudio()
    })
    .on(RoomEvent.AudioPlaybackStatusChanged, () => {
      refreshAudioState()
    })
    .on(RoomEvent.Disconnected, () => {
      // 连接没了：这条音频不可能再出声，留着状态只会让界面继续说"正在语音沟通"。
      detachTeacherAudio()
    })

  return {
    async connect(): Promise<void> {
      // §26/§28：学生端 autoSubscribe 必须为 false，且不能由调用方改写。
      await room.connect(credentials.livekitUrl, credentials.token, { autoSubscribe: false })
    },

    async publishScreenTrack(track: MediaStreamTrack): Promise<void> {
      /**
       * `userProvidedTrack = true`：告诉 SDK"这条轨道由应用提供，不要自己
       * 释放或重新采集"。这正是 §20「复用已授权的 MediaStreamTrack」在 SDK
       * 层面的表达——置为 false 时，SDK 在重连等场景会尝试重新获取媒体设备，
       * 结果就是学生在课堂中途又被弹一次授权框。
       */
      const local = new LocalVideoTrack(track, undefined, true)
      await room.localParticipant.publishTrack(local, { source: Track.Source.ScreenShare })
      publishedTrack = local
    },

    async unpublishScreenTrack(): Promise<void> {
      const local = publishedTrack
      publishedTrack = null
      if (!local) return
      // 第二个参数 false：不 stop 本地轨道，释放由捕获层负责（见上面的选项说明）。
      await room.localParticipant.unpublishTrack(local, false)
    },

    async publishCameraTrack(track: MediaStreamTrack): Promise<void> {
      /**
       * 同样是 `userProvidedTrack = true`，但这里的意义比屏幕那条更直接：
       * 摄像头在**重连**场景下如果让 SDK 自己重新采集，学生会在课堂中途再被弹一次
       * 摄像头授权框——而 §24 的整个设计前提就是"只在他点击的那一刻请求一次"。
       *
       * `name: 'camera'` 与后端契约里那条轨道的名字逐字一致：判定归属靠的是
       * `source`，但名字会进入 SFU 与 webhook 的记录，排障时能一眼区分
       * "这条是屏幕还是摄像头"（§75 的契约把两件事都写明了）。
       */
      const local = new LocalVideoTrack(track, undefined, true)
      await room.localParticipant.publishTrack(local, {
        source: Track.Source.Camera,
        name: 'camera',
      })
      publishedCameraTrack = local
    },

    async unpublishCameraTrack(): Promise<void> {
      const local = publishedCameraTrack
      publishedCameraTrack = null
      if (!local) return
      /**
       * 第二个参数 false：**不**在这里 stop。
       *
       * WHY 这条尤其重要：摄像头轨道与屏幕轨道不同，它不是"用完就一直挂着"的
       * 那种资源——`stop()` 是唯一能让系统摄像头指示灯灭掉的动作。释放点必须只有
       * 一个（采集层），否则"unpublish 成功、stop 没跑到"就会留下一个学生以为
       * 已经关掉、实际仍在采集的摄像头。store 里 unpublish 与 stop 是紧挨着的两句。
       */
      await room.localParticipant.unpublishTrack(local, false)
    },

    async publishMicrophoneTrack(track: MediaStreamTrack): Promise<void> {
      /**
       * 同样是 `userProvidedTrack = true`，理由与摄像头那条一模一样：
       * 麦克风在**重连**场景下若让 SDK 自己重新采集，学生会在课堂中途再被弹一次
       * 麦克风授权框——而 §25 的前提就是"只在他点击的那一刻请求一次"。
       *
       * `name: 'mic'` 与后端契约里那条轨道的名字逐字一致：判定归属靠 `source`，
       * 但名字会进入 SFU 与 webhook 的记录，排障时能一眼区分三条轨道。
       */
      const local = new LocalAudioTrack(track, undefined, true)
      await room.localParticipant.publishTrack(local, {
        source: Track.Source.Microphone,
        name: 'mic',
      })
      publishedMicrophoneTrack = local
    },

    async unpublishMicrophoneTrack(): Promise<void> {
      const local = publishedMicrophoneTrack
      publishedMicrophoneTrack = null
      if (!local) return
      /**
       * 第二个参数 false：**不**在这里 stop。
       *
       * WHY：`stop()` 是唯一能让系统麦克风指示灯灭掉的动作，释放点必须只有一个
       * （采集层），否则"unpublish 成功、stop 没跑到"会留下一个学生以为已经关掉、
       * 实际仍在录音的麦克风。store 里 unpublish 与 stop 是紧挨着的两句。
       */
      await room.localParticipant.unpublishTrack(local, false)
    },

    onTeacherAudioChanged(listener: (state: TeacherAudioState) => void): () => void {
      audioListeners.add(listener)
      // 立刻报一次当前状态：订阅发生在"音频已经到位"之后时，调用方不该等到下一次变化。
      listener(audioState)
      return () => {
        audioListeners.delete(listener)
      }
    },

    async resumeTeacherAudio(): Promise<boolean> {
      await room.startAudio().catch(() => undefined)
      refreshAudioState()
      return audioState === 'playing'
    },

    async disconnect(): Promise<void> {
      // stopTracks=false：同上，本地屏幕/摄像头/麦克风轨道由各自的采集层统一释放。
      detachTeacherAudio()
      return room.disconnect(false)
    },

    connectionQuality(): ConnectionQualityLevel {
      return toQualityLevel(room.localParticipant.connectionQuality)
    },

    onQualityChanged(listener: (quality: ConnectionQualityLevel) => void): () => void {
      const handler = (quality: ConnectionQuality, participant: Participant): void => {
        // 只关心本地参与者：学生端不订阅别人，别人的质量与我们无关（§26）。
        if (participant !== room.localParticipant) return
        listener(toQualityLevel(quality))
      }
      room.on(RoomEvent.ConnectionQualityChanged, handler)
      return () => room.off(RoomEvent.ConnectionQualityChanged, handler)
    },

    onDisconnected(listener: (reason: MediaDisconnectReason) => void): () => void {
      const handler = (reason?: DisconnectReason): void => {
        listener(describeDisconnectReason(reason))
      }
      room.on(RoomEvent.Disconnected, handler)
      return () => room.off(RoomEvent.Disconnected, handler)
    },

    onReconnecting(listener: () => void): () => void {
      room.on(RoomEvent.Reconnecting, listener)
      return () => room.off(RoomEvent.Reconnecting, listener)
    },

    onReconnected(listener: () => void): () => void {
      room.on(RoomEvent.Reconnected, listener)
      return () => room.off(RoomEvent.Reconnected, listener)
    },
  }
}
