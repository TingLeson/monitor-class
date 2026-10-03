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
 * 2. 只认购 `source === screen_share` 的轨道：摄像头 / 麦克风属于 Phase 9/10，
 *    这里连看都不看（老师端 Token 也只允许发布 microphone，§27）。
 * 3. 用非 deprecated 的 `RemoteTrackPublication.setSubscribed()`，
 *    而不是直接改 `publication.subscribed` 这种内部字段。
 */

import {
  Room,
  RoomEvent,
  Track,
  VideoQuality,
  type DisconnectReason,
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
   * 拆掉一条订阅：把画面从所有元素上摘下来，并 `setSubscribed(false)`。
   *
   * WHY 取消订阅而不只是 detach：让服务端**停止下行**才是省带宽的那一步（§52）。
   * 两种订阅共用这一段，避免"屏幕那边记得 setSubscribed(false)、摄像头那边忘了"
   * 这种只会在真机上表现为"摄像头一直偷偷在下行"的漏。
   */
  function dropSubscription(entry: {
    publication: RemoteTrackPublication
    track: RemoteVideoTrack
    elements: Set<HTMLVideoElement>
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
   */
  function waitForTrack(publication: RemoteTrackPublication): Promise<RemoteVideoTrack> {
    if (publication.track) return Promise.resolve(publication.track as RemoteVideoTrack)
    return new Promise<RemoteVideoTrack>((resolve, reject) => {
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
        resolve(track as RemoteVideoTrack)
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
      const track = await waitForTrack(publication)

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
      const track = await waitForTrack(publication)

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
        listener()
      }

      /**
       * 屏幕与摄像头轨道的发布/取消都要通知。
       *
       * WHY 屏幕：学生重新共享时（§22）就是"先 unpublish 再 publish"，老师端要借此
       * 把卡片从"屏幕中断"切回"有画面"。
       * WHY 摄像头（Phase 9）：`CAMERA_CHANGED` 事件（§24）是**业务**通知，它到达时
       * 这条 publication 可能还没出现在老师端的 participant 上（webhook 与 SFU 的
       * 传播本来就有先后）。不在这里再通知一次，画中画就要等到下一次快照（60 秒）
       * 才会出现——而老师盯着的是一个"学生说开了摄像头却什么都没有"的卡片。
       * 麦克风仍然不筛进来：音频属于 Phase 10（§54）。
       *
       * 判据用 `publication.source`（而不是在 participant 的 trackPublications 里找）：
       * 事件到达时 participant 的轨道表**可能还没有**这条新轨道，用后者会漏掉事件。
       */
      const isTrackedSource = (publication: RemoteTrackPublication): boolean =>
        publication.source === Track.Source.ScreenShare ||
        publication.source === Track.Source.Camera

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
         * 按 source 分开清——摄像头关掉不该把屏幕那条也拆了（反之亦然）。
         */
        if (publication.source === Track.Source.Camera) {
          dropCameraSubscription(participant.identity)
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
