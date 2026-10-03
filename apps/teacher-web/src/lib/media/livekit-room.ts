/**
 * 老师端 LiveKit 适配层（§27 / §29 / §51 / §52）。
 *
 * 整个 app 里**只有这个文件**import `livekit-client`，而且是被 `media-room.ts`
 * 的默认工厂动态加载的。
 *
 * 手动订阅在这里落地（§52 的核心）：
 *
 * ```
 * connect(url, token, { autoSubscribe: false })
 *        ↓
 * subscribeScreen(identity)
 *        ↓
 * publication.setSubscribed(true)      ← 只订阅屏幕上真的看得见的那一个
 *        ↓
 * TrackSubscribed → track.attach(video)
 * ```
 *
 * 三条硬约束：
 * 1. `autoSubscribe: false`：老师端绝不自动订阅整个房间。一个 30 人的班
 *    自动订阅就是同时下载 30 路视频（§52 明确禁止）。
 * 2. 只认购**被业务逻辑确认过**的轨道：屏幕（§29/§52 的可见性 + Focus）、
 *    摄像头（§24 的画中画）、以及**至多一个**学生的麦克风（§32 的 Focus）。
 *    三条订阅各自独立，互不牵连——这是 Phase 9/10 反复踩到的那个坑。
 * 3. 用非 deprecated 的 `RemoteTrackPublication.setSubscribed()`，
 *    而不是直接改 `publication.subscribed` 这种内部字段。
 */

import {
  LocalAudioTrack,
  Room,
  RoomEvent,
  Track,
  VideoQuality,
  type DisconnectReason,
  type RemoteAudioTrack,
  type RemoteParticipant,
  type RemoteTrack,
  type RemoteTrackPublication,
  type RemoteVideoTrack,
} from 'livekit-client'
import type {
  CameraSubscription,
  MediaCredentials,
  MediaDisconnectReason,
  MediaRemoteParticipant,
  MicrophoneSubscription,
  MonitorRoom,
  ScreenQuality,
  ScreenSubscription,
} from './media-room.ts'

/**
 * 等待订阅生效的上限。
 *
 * 手动订阅是"请求—响应"：`setSubscribed(true)` 之后要等服务端把轨道推下来。
 * 没有上限的话，一次被拒绝的订阅（权限、网络、对方中途停止共享）会让这张卡片
 * 永远停在"正在订阅…"，老师看不出到底卡在哪。超时后按失败处理并给重试入口。
 */
const SUBSCRIBE_TIMEOUT_MS = 15_000

/** 断开原因：SDK 用数字枚举，换成可读名字便于排障。 */
function describeDisconnectReason(reason: DisconnectReason | undefined): MediaDisconnectReason {
  if (reason === undefined) return null
  return String(reason)
}

/** 这条参与者有没有在发布屏幕轨道（只看 screen_share，§29）。 */
function findScreenPublication(participant: RemoteParticipant): RemoteTrackPublication | undefined {
  return findPublication(participant, Track.Source.ScreenShare)
}

/** 这条参与者有没有在发布摄像头轨道（§24）。 */
function findCameraPublication(participant: RemoteParticipant): RemoteTrackPublication | undefined {
  return findPublication(participant, Track.Source.Camera)
}

/** 这条参与者有没有在发布麦克风轨道（§32）。 */
function findMicrophonePublication(
  participant: RemoteParticipant,
): RemoteTrackPublication | undefined {
  return findPublication(participant, Track.Source.Microphone)
}

function findPublication(
  participant: RemoteParticipant,
  source: Track.Source,
): RemoteTrackPublication | undefined {
  for (const publication of participant.trackPublications.values()) {
    if (publication.source === source) return publication
  }
  return undefined
}

/**
 * 端口档位 → LiveKit 内建能力（§52：用 SDK 已有能力，不自研 RTP ABR）。
 *
 * WHY 只用 `setVideoQuality`，**不**顺带调 `setVideoDimensions`：
 * 房间开了 `adaptiveStream`，SDK 会按 `<video>` 元素的实际像素尺寸挑层；
 * 而 `setVideoDimensions` 的语义是"显式尺寸，优先于 adaptive stream"
 * （SDK 源码原话），一旦调用就等于把自适应关掉、把尺寸写死。老师拖一下窗口
 * 或改一下网格列数，写死的尺寸就变成了错误的尺寸。所以这里只封顶质量档，
 * 具体分辨率交给 SDK 按元素大小决定。
 */
function toVideoQuality(quality: ScreenQuality): VideoQuality {
  return quality === 'high' ? VideoQuality.HIGH : VideoQuality.LOW
}

export function createLiveKitMonitorRoom(credentials: MediaCredentials): MonitorRoom {
  const room = new Room({
    // §52：用 LiveKit 已有的自适应能力（按 <video> 实际尺寸选层、不可见时暂停下行），
    // 而不是自己实现一套码率算法。
    adaptiveStream: true,
    dynacast: true,
    // 关页面时 SDK 自己尽力断开。
    disconnectOnPageLeave: true,
  })

  /** 已建立的**屏幕**订阅：identity → 轨道、当前画质档与它当前挂着的元素。 */
  const subscriptions = new Map<
    string,
    {
      publication: RemoteTrackPublication
      track: RemoteVideoTrack
      elements: Set<HTMLVideoElement>
      quality: ScreenQuality
    }
  >()

  /**
   * 已建立的**摄像头**订阅（§24）。
   *
   * WHY 是第二张表而不是第一张表多一个字段：摄像头与屏幕是同一个 participant 的
   * 两条独立 publication，生命周期互不相干（学生可以只开摄像头、可以共享屏幕时
   * 中途关掉摄像头）。合并成"每人一条记录"之后，任何一侧的取消订阅都会顺手把
   * 另一侧也拆掉——那正是本 Phase 最需要避免的一种互相影响。
   *
   * 没有画质档：画中画与 Focus 的 Camera 区都是小窗，永远按 LOW 订阅。
   */
  const cameraSubscriptions = new Map<
    string,
    {
      publication: RemoteTrackPublication
      track: RemoteVideoTrack
      elements: Set<HTMLVideoElement>
    }
  >()

  /**
   * 已建立的**麦克风**订阅（§32）。
   *
   * 第三张表，理由与第二张完全相同：麦克风是同一个 participant 的第三条独立
   * publication（学生可以只开麦、可以在共享屏幕时说话、也可以在老师听的时候关掉摄像头）。
   * 合并任何两张表，都会让"取消一条订阅顺手拆掉另一条"变成一个必然发生的 bug。
   *
   * 没有画质档：音频没有分辨率的概念，§52 的分层只针对视频。
   */
  const microphoneSubscriptions = new Map<
    string,
    {
      publication: RemoteTrackPublication
      track: RemoteAudioTrack
      elements: Set<HTMLAudioElement>
    }
  >()

  /**
   * 老师自己已发布的麦克风轨道（§27/§31）。
   *
   * `LocalAudioTrack`（而不是 Video）：老师 Token 只允许发布 microphone，
   * 用错类型会在 `publishTrack` 时被 SDK 拒绝，而那时错误信息离"权限"很远。
   */
  let publishedMicrophoneTrack: LocalAudioTrack | null = null

  /**
   * 切换某条订阅的画质档（§30 / §52）。
   *
   * WHY 在这里再判一次"档位没变就返回"：SDK 的 `setVideoQuality` 内部已经去重，
   * 但它去重的是**它自己记的值**；本地记一份能让适配层的行为可被测试断言
   * （"每 10 秒一轮的轮询不会反复发信令"这件事必须看得见）。
   */
  function applyQuality(
    entry: { publication: RemoteTrackPublication; quality: ScreenQuality },
    quality: ScreenQuality,
  ): void {
    if (entry.quality === quality) return
    entry.quality = quality
    entry.publication.setVideoQuality(toVideoQuality(quality))
  }

  /**
   * 拆掉一条订阅：把画面/声音从所有元素上摘下来，并 `setSubscribed(false)`。
   *
   * WHY 取消订阅而不只是 detach：让服务端**停止下行**才是省带宽的那一步（§52）。
   * 三种订阅共用这一段（元素类型放宽成 `HTMLMediaElement`：视频挂 `<video>`、
   * 音频挂 `<audio>`，而"摘下来 + 退订"这两步对两者完全一样）。
   */
  function dropSubscription(entry: {
    publication: RemoteTrackPublication
    track: RemoteTrack
    elements: Set<HTMLMediaElement>
  }): void {
    for (const element of [...entry.elements]) entry.track.detach(element)
    entry.elements.clear()
    entry.publication.setSubscribed(false)
  }

  /**
   * 等一条轨道真的被订阅下来。
   *
   * 监听房间级的 `TrackSubscribed` 而不是 publication 自己的事件：
   * 后者在部分版本里会漏事件，而房间级事件始终会到（它就是 SDK 通知应用的入口）。
   *
   * 返回 `RemoteTrack`（不区分音视频）：三条订阅路径都要用它，而各自的调用点
   * 比谁都清楚自己在等的是哪一种轨道。
   */
  function waitForTrack(publication: RemoteTrackPublication): Promise<RemoteTrack> {
    if (publication.track) return Promise.resolve(publication.track)
    return new Promise<RemoteTrack>((resolve, reject) => {
      const cleanup = (): void => {
        clearTimeout(timer)
        room.off(RoomEvent.TrackSubscribed, onSubscribed)
        room.off(RoomEvent.TrackSubscriptionFailed, onFailed)
      }
      const timer = setTimeout(() => {
        cleanup()
        reject(new Error('subscribe-timeout'))
      }, SUBSCRIBE_TIMEOUT_MS)
      const onSubscribed = (track: RemoteTrack, subscribed: RemoteTrackPublication): void => {
        if (subscribed.trackSid !== publication.trackSid) return
        cleanup()
        resolve(track)
      }
      const onFailed = (trackSid: string): void => {
        if (trackSid !== publication.trackSid) return
        cleanup()
        reject(new Error('subscribe-failed'))
      }
      room.on(RoomEvent.TrackSubscribed, onSubscribed)
      room.on(RoomEvent.TrackSubscriptionFailed, onFailed)
    })
  }

  /** 订阅对象：attach/detach/setQuality 只操作这个 identity 的轨道。 */
  function toSubscription(
    identity: string,
    entry: {
      publication: RemoteTrackPublication
      track: RemoteVideoTrack
      elements: Set<HTMLVideoElement>
      quality: ScreenQuality
    },
  ): ScreenSubscription {
    return {
      identity,
      attach(element: HTMLVideoElement): () => void {
        // SDK 的 attach 会同时设置 srcObject 与 autoplay/playsInline 处理。
        entry.track.attach(element)
        entry.elements.add(element)
        return () => {
          entry.track.detach(element)
          entry.elements.delete(element)
        }
      },
      setQuality(quality: ScreenQuality): void {
        applyQuality(entry, quality)
      },
    }
  }

  /** 摄像头订阅对象（§24）：只有 attach/detach，没有画质档位。 */
  function toCameraSubscription(
    identity: string,
    entry: {
      publication: RemoteTrackPublication
      track: RemoteVideoTrack
      elements: Set<HTMLVideoElement>
    },
  ): CameraSubscription {
    return {
      identity,
      attach(element: HTMLVideoElement): () => void {
        entry.track.attach(element)
        entry.elements.add(element)
        return () => {
          entry.track.detach(element)
          entry.elements.delete(element)
        }
      },
    }
  }

  /**
   * 麦克风订阅对象（§32）：与摄像头同形，但挂的是 `<audio>`。
   *
   * 元素类型必须分开：`<video srcObject=audioTrack>` 在部分浏览器里能响、在另一些
   * 里完全无声——一个"在老师的机器上能用"的音频实现是最难被发现的一类缺陷。
   */
  function toMicrophoneSubscription(
    identity: string,
    entry: {
      publication: RemoteTrackPublication
      track: RemoteAudioTrack
      elements: Set<HTMLAudioElement>
    },
  ): MicrophoneSubscription {
    return {
      identity,
      attach(element: HTMLAudioElement): () => void {
        entry.track.attach(element)
        entry.elements.add(element)
        return () => {
          entry.track.detach(element)
          entry.elements.delete(element)
        }
      },
    }
  }

  return {
    async connect(): Promise<void> {
      // §52：老师端 autoSubscribe 必须为 false，且不能由调用方改写。
      await room.connect(credentials.livekitUrl, credentials.token, { autoSubscribe: false })
    },

    async disconnect(): Promise<void> {
      for (const [identity, entry] of [...subscriptions]) {
        dropSubscription(entry)
        subscriptions.delete(identity)
      }
      for (const [identity, entry] of [...cameraSubscriptions]) {
        dropSubscription(entry)
        cameraSubscriptions.delete(identity)
      }
      for (const [identity, entry] of [...microphoneSubscriptions]) {
        dropSubscription(entry)
        microphoneSubscriptions.delete(identity)
      }
      publishedMicrophoneTrack = null
      await room.disconnect(true)
    },

    participants(): MediaRemoteParticipant[] {
      return [...room.remoteParticipants.values()].map((participant) => ({
        identity: participant.identity,
        hasScreen: findScreenPublication(participant) !== undefined,
      }))
    },

    async subscribeScreen(
      identity: string,
      quality: ScreenQuality = 'low',
    ): Promise<ScreenSubscription | null> {
      /**
       * 幂等保护的第一层：已经订阅过就直接返回同一个订阅。
       * 监督墙每 10 秒刷新一次业务状态，而"屏幕仍在共享"每次都会成立——
       * 没有这一层，每 10 秒就会重新 `setSubscribed(true)` 一次。
       *
       * 已经订阅过时仍然应用一次 `quality`：调用方（store）才是"这个人现在该看多清楚"
       * 的权威（Focus 进出会改这个判断），而 applyQuality 自己会去重。
       */
      const existing = subscriptions.get(identity)
      if (existing) {
        applyQuality(existing, quality)
        return toSubscription(identity, existing)
      }

      const participant = room.getParticipantByIdentity(identity) as RemoteParticipant | undefined
      if (!participant) return null
      const publication = findScreenPublication(participant)
      if (!publication) return null

      /**
       * 顺序不能反：**先** `setSubscribed(true)`，**再** `setVideoQuality(...)`。
       *
       * WHY：`autoSubscribe = false` 时 publication 的 `subscribed` 初始值就是 false，
       * 而 SDK 的 `setVideoQuality` 有一道 `isDesired`（= `subscribed !== false`）门槛——
       * 未订阅时它直接返回。先设画质等于什么都没设，网格会按默认（最高）层推流，
       * 正是 §52 要避免的那一刻。
       *
       * 那"第一秒按高码率推"呢？两条信令在同一个 tick 里发出，SFU 要在收到订阅之后
       * 才开始推流，所以不存在"先推一秒 1080p 再降档"的窗口；§52 要防的是**持续**的
       * 高码率下行，那由画质档位本身负责。
       */
      if (!publication.isSubscribed) publication.setSubscribed(true)
      publication.setVideoQuality(toVideoQuality(quality))
      const track = (await waitForTrack(publication)) as RemoteVideoTrack

      // 等待期间可能已经被取消（学生停止共享 / 老师关掉了这个 tile）。
      if (!publication.isSubscribed) return null

      const entry = {
        publication,
        track,
        elements: new Set<HTMLVideoElement>(),
        quality,
      }
      subscriptions.set(identity, entry)
      return toSubscription(identity, entry)
    },

    async unsubscribeScreen(identity: string): Promise<void> {
      const entry = subscriptions.get(identity)
      if (!entry) return
      subscriptions.delete(identity)
      dropSubscription(entry)
    },

    async subscribeCamera(identity: string): Promise<CameraSubscription | null> {
      // 幂等：监督墙每次刷新都会重算计划，"学生摄像头还开着"每次都会成立。
      const existing = cameraSubscriptions.get(identity)
      if (existing) return toCameraSubscription(identity, existing)

      const participant = room.getParticipantByIdentity(identity) as RemoteParticipant | undefined
      if (!participant) return null
      const publication = findCameraPublication(participant)
      if (!publication) return null

      /**
       * 顺序与屏幕那条完全一致：先订阅、再定画质。
       *
       * §52 要求网格优先低分辨率，而画中画比网格卡片还小——它永远用 SDK 的最低档。
       * 这里**不**调用 `setVideoDimensions`：房间开了 adaptiveStream，SDK 会按
       * `<video>` 元素的实际尺寸挑层；写死尺寸会在老师改变窗口大小后变成错误的尺寸。
       */
      if (!publication.isSubscribed) publication.setSubscribed(true)
      publication.setVideoQuality(VideoQuality.LOW)
      const track = (await waitForTrack(publication)) as RemoteVideoTrack

      // 等待期间可能已经被取消（学生关了摄像头 / 卡片滚出视口 / 页面切走了）。
      if (!publication.isSubscribed) return null

      const entry = { publication, track, elements: new Set<HTMLVideoElement>() }
      cameraSubscriptions.set(identity, entry)
      return toCameraSubscription(identity, entry)
    },

    async unsubscribeCamera(identity: string): Promise<void> {
      const entry = cameraSubscriptions.get(identity)
      if (!entry) return
      cameraSubscriptions.delete(identity)
      // 只动摄像头那条：屏幕订阅与它没有任何关系。
      dropSubscription(entry)
    },

    /* ---------------------------------------------------------------------- */
    /* 老师自己的麦克风（§27/§31）                                             */
    /* ---------------------------------------------------------------------- */

    async publishMicrophoneTrack(track: MediaStreamTrack): Promise<void> {
      /**
       * `userProvidedTrack = true`：与另外两条本地轨道同一条纪律——重连时 SDK
       * 不得自己重新采集麦克风，否则老师会在课堂中途被弹第二次授权框。
       *
       * `name: 'mic'` 与后端契约里那条轨迹的名字逐字一致（排障时能一眼区分
       * 三条轨道；归属仍然靠 `source`）。
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
      // 第二个参数 false：不 stop 本地轨道，释放设备由采集层负责（指示灯才会灭）。
      await room.localParticipant.unpublishTrack(local, false)
    },

    /* ---------------------------------------------------------------------- */
    /* 听学生的麦克风（§32）                                                   */
    /* ---------------------------------------------------------------------- */

    async subscribeMicrophone(identity: string): Promise<MicrophoneSubscription | null> {
      // 幂等：Focus 里每 10 秒一轮的刷新、以及 MIC_CHANGED 事件都会重算计划。
      const existing = microphoneSubscriptions.get(identity)
      if (existing) return toMicrophoneSubscription(identity, existing)

      const participant = room.getParticipantByIdentity(identity) as RemoteParticipant | undefined
      if (!participant) return null
      const publication = findMicrophonePublication(participant)
      if (!publication) return null

      /**
       * **不**调用 `setVideoQuality`：音频没有分辨率。§52 的"网格低 / Focus 高"
       * 是视频的概念，给一条 audio publication 设画质要么被 SDK 忽略、
       * 要么在将来的版本里变成一个无意义的信令。
       */
      if (!publication.isSubscribed) publication.setSubscribed(true)
      /**
       * 自动播放策略：老师点开 Focus / 开自己的麦克风都发生在用户手势之后的页面里，
       * 所以这里顺手 `startAudio()` 通常直接成功。失败不抛——它只意味着老师需要
       * 再点一次页面，而这条路不该把整个订阅流程打断。
       */
      void room.startAudio().catch(() => undefined)
      const track = (await waitForTrack(publication)) as RemoteAudioTrack

      // 等待期间可能已经被取消（学生关麦 / 退出 Focus / 切走标签页）。
      if (!publication.isSubscribed) return null

      const entry = { publication, track, elements: new Set<HTMLAudioElement>() }
      microphoneSubscriptions.set(identity, entry)
      return toMicrophoneSubscription(identity, entry)
    },

    async unsubscribeMicrophone(identity: string): Promise<void> {
      const entry = microphoneSubscriptions.get(identity)
      if (!entry) return
      microphoneSubscriptions.delete(identity)
      // 只动音频那条：屏幕与摄像头一个都不碰（§32 的隔离）。
      dropSubscription(entry)
    },

    onParticipantsChanged(listener: () => void): () => void {
      /** 丢掉某个 identity 的本地**屏幕**订阅记录，并把画面从所有元素上摘下来。 */
      const dropScreenSubscription = (identity: string): void => {
        const entry = subscriptions.get(identity)
        if (!entry) return
        for (const element of [...entry.elements]) entry.track.detach(element)
        entry.elements.clear()
        subscriptions.delete(identity)
      }

      /** 摄像头那一半（§24）：同上，但只动摄像头。 */
      const dropCameraSubscription = (identity: string): void => {
        const entry = cameraSubscriptions.get(identity)
        if (!entry) return
        for (const element of [...entry.elements]) entry.track.detach(element)
        entry.elements.clear()
        cameraSubscriptions.delete(identity)
      }

      /** 麦克风那一半（§32）：同上，但只动音频。 */
      const dropMicrophoneSubscription = (identity: string): void => {
        const entry = microphoneSubscriptions.get(identity)
        if (!entry) return
        for (const element of [...entry.elements]) entry.track.detach(element)
        entry.elements.clear()
        microphoneSubscriptions.delete(identity)
      }

      const onParticipantConnected = (): void => listener()
      /**
       * 参与者离开（学生关掉页面、网络断了、老师把他移出课堂……）。
       *
       * WHY 必须在这里清掉本地订阅记录：参与者走了之后 publication 已经不会再推数据，
       * 但我们的 map 里还留着它——下一次 `subscribeScreen` 会**直接返回这条死订阅**，
       * 卡片就会一直停着最后一帧画面。监督墙最不能出现的就是"看起来还在，其实早走了"。
       * 摄像头同理：留着就是右下角一个僵死的画中画。
       */
      const onParticipantDisconnected = (participant: RemoteParticipant): void => {
        dropScreenSubscription(participant.identity)
        dropCameraSubscription(participant.identity)
        /**
         * 麦克风同样必须在这里清掉（§32）：参与者走了之后那条 publication 不会再推
         * 数据，而 map 里留着它，下一次 `subscribeMicrophone` 会**直接返回这条死订阅**
         * ——老师的耳机里从此一片安静，而 Focus 面板还写着"正在听该学生的麦克风"。
         */
        dropMicrophoneSubscription(participant.identity)
        listener()
      }

      /**
       * 屏幕、摄像头与麦克风轨道的发布/取消都要通知。
       *
       * WHY 屏幕：学生重新共享时（§22）就是"先 unpublish 再 publish"，老师端要借此
       * 把卡片从"屏幕中断"切回"有画面"。
       * WHY 摄像头（Phase 9）：`CAMERA_CHANGED` 事件（§24）是**业务**通知，它到达时
       * 这条 publication 可能还没出现在老师端的 participant 上（webhook 与 SFU 的
       * 传播本来就有先后）。不在这里再通知一次，画中画就要等到下一次快照（60 秒）
       * 才会出现——而老师盯着的是一个"学生说开了摄像头却什么都没有"的卡片。
       * WHY 麦克风（Phase 10）：同一条理由，代价更直接——`MIC_CHANGED` 先到、
       * 轨道后到时，Focus 面板会显示"麦克风已开启"却听不到任何声音，
       * 而老师会以为自己耳机坏了。
       *
       * 判据用 `publication.source`（而不是在 participant 的 trackPublications 里找）：
       * 事件到达时 participant 的轨道表**可能还没有**这条新轨道，用后者会漏掉事件。
       */
      const isTrackedSource = (publication: RemoteTrackPublication): boolean =>
        publication.source === Track.Source.ScreenShare ||
        publication.source === Track.Source.Camera ||
        publication.source === Track.Source.Microphone

      const onTrackPublished = (publication: RemoteTrackPublication): void => {
        if (!isTrackedSource(publication)) return
        listener()
      }
      const onTrackUnpublished = (
        publication: RemoteTrackPublication,
        participant: RemoteParticipant,
      ): void => {
        if (!isTrackedSource(publication)) return
        /**
         * 轨道没了：把对应的本地订阅记录清掉，否则下次订阅会返回一条死轨道。
         * 按 source 分开清——关麦克风不该把屏幕或摄像头那条也拆了（反之亦然）。
         */
        if (publication.source === Track.Source.Camera) {
          dropCameraSubscription(participant.identity)
        } else if (publication.source === Track.Source.Microphone) {
          dropMicrophoneSubscription(participant.identity)
        } else {
          dropScreenSubscription(participant.identity)
        }
        listener()
      }

      room.on(RoomEvent.ParticipantConnected, onParticipantConnected)
      room.on(RoomEvent.ParticipantDisconnected, onParticipantDisconnected)
      room.on(RoomEvent.TrackPublished, onTrackPublished)
      room.on(RoomEvent.TrackUnpublished, onTrackUnpublished)
      return () => {
        room.off(RoomEvent.ParticipantConnected, onParticipantConnected)
        room.off(RoomEvent.ParticipantDisconnected, onParticipantDisconnected)
        room.off(RoomEvent.TrackPublished, onTrackPublished)
        room.off(RoomEvent.TrackUnpublished, onTrackUnpublished)
      }
    },

    onScreenSubscribed(listener: (identity: string) => void): () => void {
      const handler = (
        _track: RemoteTrack,
        publication: RemoteTrackPublication,
        participant: RemoteParticipant,
      ): void => {
        // **必须按 source 过滤**：房间级的 TrackSubscribed 对**每一条**轨道都触发，
        // 摄像头也在内。不过滤的话，"订阅到摄像头"会被上层当成"订阅到屏幕"，
        // 而摄像头是 optional 的（§21）——两个事实混在一起就会互相拆台。
        if (publication.source !== Track.Source.ScreenShare) return
        listener(participant.identity)
      }
      room.on(RoomEvent.TrackSubscribed, handler)
      return () => room.off(RoomEvent.TrackSubscribed, handler)
    },

    onScreenUnsubscribed(listener: (identity: string) => void): () => void {
      const handler = (
        _track: RemoteTrack,
        publication: RemoteTrackPublication,
        participant: RemoteParticipant,
      ): void => {
        // 同上：**取消订阅摄像头不等于屏幕没了**。少了这行过滤的真实后果是：
        // 学生一关摄像头，老师端卡片的屏幕画面会一起消失（只剩"正在订阅画面…"），
        // 而业务徽章仍显示 🟢——画面与状态自相矛盾，老师无法判断到底出了什么事。
        if (publication.source !== Track.Source.ScreenShare) return
        listener(participant.identity)
      }
      room.on(RoomEvent.TrackUnsubscribed, handler)
      return () => room.off(RoomEvent.TrackUnsubscribed, handler)
    },

    onDisconnected(listener: (reason: MediaDisconnectReason) => void): () => void {
      const handler = (reason?: DisconnectReason): void => {
        listener(describeDisconnectReason(reason))
      }
      room.on(RoomEvent.Disconnected, handler)
      return () => room.off(RoomEvent.Disconnected, handler)
    },
  }
}
