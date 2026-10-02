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
  for (const publication of participant.trackPublications.values()) {
    if (publication.source === Track.Source.ScreenShare) return publication
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

  /** 已建立的订阅：identity → 轨道、当前画质档与它当前挂着的元素。 */
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

  return {
    async connect(): Promise<void> {
      // §52：老师端 autoSubscribe 必须为 false，且不能由调用方改写。
      await room.connect(credentials.livekitUrl, credentials.token, { autoSubscribe: false })
    },

    async disconnect(): Promise<void> {
      for (const [identity, entry] of [...subscriptions]) {
        for (const element of [...entry.elements]) entry.track.detach(element)
        entry.elements.clear()
        entry.publication.setSubscribed(false)
        subscriptions.delete(identity)
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
      for (const element of [...entry.elements]) entry.track.detach(element)
      entry.elements.clear()
      // 取消订阅（而不是只停止播放）：让服务端不再往下推，这才是省带宽的那一步（§52）。
      entry.publication.setSubscribed(false)
    },

    onParticipantsChanged(listener: () => void): () => void {
      /** 丢掉某个 identity 的本地订阅记录，并把画面从所有元素上摘下来。 */
      const dropSubscription = (identity: string): void => {
        const entry = subscriptions.get(identity)
        if (!entry) return
        for (const element of [...entry.elements]) entry.track.detach(element)
        entry.elements.clear()
        subscriptions.delete(identity)
      }

      const onParticipantConnected = (): void => listener()
      /**
       * 参与者离开（学生关掉页面、网络断了、老师把他移出课堂……）。
       *
       * WHY 必须在这里清掉本地订阅记录：参与者走了之后 publication 已经不会再推数据，
       * 但我们的 map 里还留着它——下一次 `subscribeScreen` 会**直接返回这条死订阅**，
       * 卡片就会一直停着最后一帧画面。监督墙最不能出现的就是"看起来还在，其实早走了"。
       */
      const onParticipantDisconnected = (participant: RemoteParticipant): void => {
        dropSubscription(participant.identity)
        listener()
      }

      /**
       * 屏幕轨道的发布/取消也要通知：学生重新共享时（§22）就是"先 unpublish 再 publish"，
       * 老师端要借此把卡片从"屏幕中断"切回"有画面"。
       *
       * 判据用 `publication.source`（而不是在 participant 的 trackPublications 里找）：
       * 事件到达时 participant 的轨道表**可能还没有**这条新轨道，用后者会漏掉事件。
       * 只筛 screen_share：摄像头/麦克风事件与本 Phase 无关（§54）。
       */
      const onTrackPublished = (publication: RemoteTrackPublication): void => {
        if (publication.source !== Track.Source.ScreenShare) return
        listener()
      }
      const onTrackUnpublished = (
        publication: RemoteTrackPublication,
        participant: RemoteParticipant,
      ): void => {
        if (publication.source !== Track.Source.ScreenShare) return
        // 轨道没了：把本地订阅记录清掉，否则下次 subscribeScreen 会返回一条死订阅。
        dropSubscription(participant.identity)
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
        _publication: RemoteTrackPublication,
        participant: RemoteParticipant,
      ): void => {
        listener(participant.identity)
      }
      room.on(RoomEvent.TrackSubscribed, handler)
      return () => room.off(RoomEvent.TrackSubscribed, handler)
    },

    onScreenUnsubscribed(listener: (identity: string) => void): () => void {
      const handler = (
        _track: RemoteTrack,
        _publication: RemoteTrackPublication,
        participant: RemoteParticipant,
      ): void => {
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
