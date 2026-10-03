import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createLiveKitMonitorRoom } from '../media/livekit-room.ts'

/**
 * 真实 SDK 适配层的钉桩测试（§27 / §29 / §52）。
 *
 * 这一份把"老师端到底怎么用 LiveKit"钉死：
 *
 * - `connect(url, token, { autoSubscribe: false })`（§52 的硬要求）；
 * - 手动订阅只认 `screen_share`，且用非 deprecated 的
 *   `RemoteTrackPublication.setSubscribed(true)`；
 * - 已订阅的 participant **不重复** `setSubscribed`（监督墙每 10 秒刷新一次，
 *   没有这层幂等就是一场订阅风暴）；
 * - 取消订阅用 `setSubscribed(false)`（真的让服务端停止下行），并 detach 元素。
 */

const lk = vi.hoisted(() => ({
  roomOptions: [] as Record<string, unknown>[],
  connectCalls: [] as { url: string; token: string; options: Record<string, unknown> }[],
  disconnectCalls: [] as (boolean | undefined)[],
  publishCalls: [] as { track: unknown; options: Record<string, unknown> }[],
  unpublishCalls: [] as { track: unknown; stop: boolean | undefined }[],
  localAudioTrackArgs: [] as unknown[][],
  startAudioCalls: 0,
  listeners: new Map<string, Set<(...args: never[]) => void>>(),
  instances: [] as { remoteParticipants: Map<string, unknown> }[],
}))

/** publication 替身：记录 setSubscribed / setVideoQuality 的调用。 */
interface FakePublication {
  trackSid: string
  source: string
  /** 与 SDK 一致：`autoSubscribe = false` 时初始值就是 false（即"未订阅"）。 */
  isSubscribed: boolean
  track?: unknown
  subscribedCalls: boolean[]
  /** 每次生效的画质调整（§52：网格 LOW / Focus HIGH）。 */
  qualityCalls: number[]
  /** 被 SDK 门槛丢弃的画质调整——非空就说明调用顺序写反了。 */
  droppedQualityCalls: number[]
}

function makePublication(sid: string, source: string): FakePublication {
  const publication: FakePublication = {
    trackSid: sid,
    source,
    isSubscribed: false,
    subscribedCalls: [],
    qualityCalls: [],
    droppedQualityCalls: [],
  }
  return Object.assign(publication, {
    setSubscribed(value: boolean): void {
      publication.subscribedCalls.push(value)
      publication.isSubscribed = value
    },
    setVideoQuality(quality: number): void {
      /**
       * 照抄真实 SDK 的门槛：`isDesired` 为假（未订阅）时 setVideoQuality 直接返回
       * （源码里的 `isManualOperationAllowed`）。没有这一条，"先设画质再订阅"这种
       * 顺序错误在测试里就永远不会暴露。
       */
      if (!publication.isSubscribed) {
        publication.droppedQualityCalls.push(quality)
        return
      }
      // 真实 SDK 内部也会去重；替身只记录调用，让"没有多余的切换"可被断言。
      if (publication.qualityCalls.at(-1) === quality) return
      publication.qualityCalls.push(quality)
    },
    setEnabled(): void {
      // 按可见性暂停下行由 LiveKit 的 adaptiveStream 负责，我们不直接调用它。
    },
  })
}

/** 远端参与者替身：只有 identity 与 trackPublications（够适配层用）。 */
function makeParticipant(identity: string, publications: FakePublication[]): unknown {
  const map = new Map<string, FakePublication>()
  for (const publication of publications) map.set(publication.trackSid, publication)
  return { identity, trackPublications: map }
}

vi.mock('livekit-client', () => {
  /** 老师端唯一允许发布的本地轨道类型（§27：canPublishSources = microphone）。 */
  class LocalAudioTrack {
    readonly kind = 'audio'
    constructor(...args: unknown[]) {
      lk.localAudioTrackArgs.push(args)
    }
  }

  class Room {
    readonly remoteParticipants = new Map<string, unknown>()

    readonly localParticipant = {
      connectionQuality: 'good',
      trackPublications: new Map<string, unknown>(),
      publishTrack: (track: unknown, options: Record<string, unknown>) => {
        lk.publishCalls.push({ track, options })
        return Promise.resolve({ trackSid: 'TR_published' })
      },
      unpublishTrack: (track: unknown, stop?: boolean) => {
        lk.unpublishCalls.push({ track, stop })
        return Promise.resolve(undefined)
      },
    }

    constructor(options: Record<string, unknown>) {
      lk.roomOptions.push(options)
      lk.instances.push(this as unknown as { remoteParticipants: Map<string, unknown> })
    }

    connect(url: string, token: string, options: Record<string, unknown>): Promise<void> {
      lk.connectCalls.push({ url, token, options })
      return Promise.resolve()
    }

    disconnect(stop?: boolean): Promise<void> {
      lk.disconnectCalls.push(stop)
      return Promise.resolve()
    }

    /** 老师端订阅学生麦克风时会顺手恢复音频播放（§32 的自动播放策略）。 */
    startAudio(): Promise<void> {
      lk.startAudioCalls += 1
      return Promise.resolve()
    }

    on(event: string, handler: (...args: never[]) => void): this {
      const set = lk.listeners.get(event) ?? new Set()
      set.add(handler)
      lk.listeners.set(event, set)
      return this
    }

    off(event: string, handler: (...args: never[]) => void): this {
      lk.listeners.get(event)?.delete(handler)
      return this
    }

    getParticipantByIdentity(identity: string): unknown {
      return this.remoteParticipants.get(identity)
    }
  }

  return {
    Room,
    LocalAudioTrack,
    RoomEvent: {
      ParticipantConnected: 'participantConnected',
      ParticipantDisconnected: 'participantDisconnected',
      TrackPublished: 'trackPublished',
      TrackUnpublished: 'trackUnpublished',
      TrackSubscribed: 'trackSubscribed',
      TrackUnsubscribed: 'trackUnsubscribed',
      TrackSubscriptionFailed: 'trackSubscriptionFailed',
      Disconnected: 'disconnected',
    },
    Track: {
      Source: { ScreenShare: 'screen_share', Camera: 'camera', Microphone: 'microphone' },
    },
    // §52 的画质分层：适配层必须用 SDK 内建的 VideoQuality，而不是自研 RTP ABR。
    VideoQuality: { LOW: 0, MEDIUM: 1, HIGH: 2 },
  }
})

function emit(event: string, ...args: unknown[]): void {
  for (const handler of [...(lk.listeners.get(event) ?? [])]) {
    ;(handler as (...rest: unknown[]) => void)(...args)
  }
}

/** 把一条参与者放进房间里（模拟他已经连上并发布了轨道）。 */
function joinParticipant(identity: string, publications: FakePublication[]): void {
  const room = lk.instances[0]
  const participant = makeParticipant(identity, publications)
  room?.remoteParticipants.set(identity, participant)
}

/** 一次成功的订阅：setSubscribed(true) 之后服务端把轨道推下来。 */
function deliverTrack(publication: FakePublication): void {
  const track = {
    attach: vi.fn(),
    detach: vi.fn(),
  }
  publication.track = track
  emit('trackSubscribed', track, publication, { identity: identityOf(publication) })
}

/** 测试里只用一个参与者，这里直接查表拿 identity。 */
function identityOf(publication: FakePublication): string {
  const room = lk.instances[0]
  for (const [identity, participant] of room?.remoteParticipants ?? []) {
    const map = (participant as { trackPublications: Map<string, FakePublication> })
      .trackPublications
    if (map.get(publication.trackSid) === publication) return identity
  }
  return 'unknown'
}

const CREDENTIALS = {
  livekitUrl: 'wss://classwatch-test.livekit.cloud',
  token: 'teacher-token',
}

describe('LiveKit 适配层（老师端）', () => {
  beforeEach(() => {
    lk.roomOptions.length = 0
    lk.connectCalls.length = 0
    lk.disconnectCalls.length = 0
    lk.publishCalls.length = 0
    lk.unpublishCalls.length = 0
    lk.localAudioTrackArgs.length = 0
    lk.startAudioCalls = 0
    lk.listeners.clear()
    lk.instances.length = 0
  })

  it('§52：connect 必须显式 autoSubscribe=false（绝不自动下载全班画面）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)

    await room.connect()

    expect(lk.connectCalls).toHaveLength(1)
    expect(lk.connectCalls[0]?.options).toEqual({ autoSubscribe: false })
    expect(lk.connectCalls[0]?.url).toBe('wss://classwatch-test.livekit.cloud')
  })

  it('房间选项使用 LiveKit 既有的自适应能力（§52）', () => {
    createLiveKitMonitorRoom(CREDENTIALS)

    expect(lk.roomOptions[0]).toEqual({
      adaptiveStream: true,
      dynacast: true,
      disconnectOnPageLeave: true,
    })
  })

  it('participants() 只报告媒体事实：identity + 有没有屏幕轨道', () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    const camera = makePublication('TR_camera', 'camera')
    joinParticipant('session-1', [screen, camera])
    joinParticipant('session-2', [camera])

    expect(room.participants()).toEqual([
      { identity: 'session-1', hasScreen: true },
      { identity: 'session-2', hasScreen: false },
    ])
  })

  it('§52：subscribeScreen 用 setSubscribed(true) 手动订阅屏幕轨道，并把画面挂到 <video>', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    joinParticipant('session-1', [screen])

    const pending = room.subscribeScreen('session-1')
    expect(screen.subscribedCalls).toEqual([true])

    deliverTrack(screen)
    const subscription = await pending
    expect(subscription?.identity).toBe('session-1')

    const element = { srcObject: null } as unknown as HTMLVideoElement
    const detach = subscription?.attach(element)
    const track = screen.track as {
      attach: ReturnType<typeof vi.fn>
      detach: ReturnType<typeof vi.fn>
    }
    expect(track.attach).toHaveBeenCalledWith(element)

    detach?.()
    expect(track.detach).toHaveBeenCalledWith(element)
  })

  it('§52：订阅时的画质档默认 LOW，且画质必须在 setSubscribed 之后下发', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    joinParticipant('session-1', [screen])

    const pending = room.subscribeScreen('session-1')

    // VideoQuality.LOW = 0（替身按 SDK 枚举取值）。
    expect(screen.subscribedCalls).toEqual([true])
    expect(screen.qualityCalls).toEqual([0])
    // 顺序写反的话画质会被 SDK 丢掉，这一条就是那个坑的守卫。
    expect(screen.droppedQualityCalls).toEqual([])

    deliverTrack(screen)
    await pending
  })

  it('§30：Focus 用 HIGH 订阅，并且可以事后降回 LOW（同一条订阅，不是重订）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    joinParticipant('session-1', [screen])

    const pending = room.subscribeScreen('session-1', 'high')
    expect(screen.subscribedCalls).toEqual([true])
    expect(screen.qualityCalls).toEqual([2])
    expect(screen.droppedQualityCalls).toEqual([])
    deliverTrack(screen)
    const subscription = await pending

    subscription?.setQuality('low')
    expect(screen.qualityCalls).toEqual([2, 0])
    expect(screen.subscribedCalls).toEqual([true])

    // 幂等：重复切到同一档不会再发一次信令。
    subscription?.setQuality('low')
    expect(screen.qualityCalls).toEqual([2, 0])
  })

  it('§52：已订阅的 participant 再次订阅时只调整画质，不会重新 setSubscribed', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    joinParticipant('session-1', [screen])
    const pending = room.subscribeScreen('session-1')
    deliverTrack(screen)
    await pending

    // 老师点开 Focus：store 会带着 high 再问一次（适配层复用手上的订阅）。
    await room.subscribeScreen('session-1', 'high')

    expect(screen.subscribedCalls).toEqual([true])
    expect(screen.qualityCalls).toEqual([0, 2])
  })

  it('§52：同一个 participant 重复订阅不会再次 setSubscribed（10 秒一轮的刷新不会变成订阅风暴）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    joinParticipant('session-1', [screen])

    const first = room.subscribeScreen('session-1')
    deliverTrack(screen)
    const subscription = await first

    const second = await room.subscribeScreen('session-1')

    expect(screen.subscribedCalls).toEqual([true])
    expect(second?.identity).toBe(subscription?.identity)
  })

  it('没有屏幕发布 / 没有这个参与者 → 返回 null（界面显示"等待共享"而不是报错）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    joinParticipant('session-1', [makePublication('TR_camera', 'camera')])

    expect(await room.subscribeScreen('session-1')).toBeNull()
    expect(await room.subscribeScreen('nobody')).toBeNull()
  })

  it('取消订阅：setSubscribed(false) 并 detach 已挂载的元素（§52 的省带宽那一步）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    joinParticipant('session-1', [screen])
    const pending = room.subscribeScreen('session-1')
    deliverTrack(screen)
    const subscription = await pending

    const element = { srcObject: null } as unknown as HTMLVideoElement
    subscription?.attach(element)
    await room.unsubscribeScreen('session-1')

    expect(screen.subscribedCalls).toEqual([true, false])
    const track = screen.track as { detach: ReturnType<typeof vi.fn> }
    expect(track.detach).toHaveBeenCalledWith(element)
  })

  it('参与者事件驱动订阅协调：加入/离开、屏幕/摄像头/麦克风轨道的发布与取消都会通知', () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    let changes = 0
    room.onParticipantsChanged(() => {
      changes += 1
    })

    emit('participantConnected', { identity: 'session-1' })
    emit('participantDisconnected', { identity: 'session-1' })
    // 注意：事件到达时 participant 的轨道表可能还没有这条新轨道——
    // 适配层因此只看 publication.source（这条断言就是在守这一点）。
    emit('trackPublished', makePublication('TR_screen', 'screen_share'), {
      identity: 'session-1',
      trackPublications: new Map(),
    })
    emit('trackUnpublished', makePublication('TR_screen', 'screen_share'), {
      identity: 'session-1',
    })
    /**
     * §24：摄像头轨道同样要通知。`CAMERA_CHANGED` 是业务事件，它到达时这条 publication
     * 可能还没出现在老师端（webhook 比 SFU 快），所以"轨道到了"这一下必须能触发协调——
     * 否则画中画要等下一次快照（60 秒）才出现。
     */
    emit('trackPublished', makePublication('TR_camera', 'camera'), {
      identity: 'session-2',
      trackPublications: new Map([['TR_camera', makePublication('TR_camera', 'camera')]]),
    })
    emit('trackUnpublished', makePublication('TR_camera', 'camera'), { identity: 'session-2' })
    /**
     * §32（Phase 10）：麦克风轨道也一样。`MIC_CHANGED` 先到、轨道后到时，
     * Focus 面板会显示"麦克风已开启"却听不到任何声音——老师会以为耳机坏了。
     * 这条通知是"声音真的可以订了"那一刻。
     */
    emit('trackPublished', makePublication('TR_mic', 'microphone'), {
      identity: 'session-3',
      trackPublications: new Map([['TR_mic', makePublication('TR_mic', 'microphone')]]),
    })
    emit('trackUnpublished', makePublication('TR_mic', 'microphone'), { identity: 'session-3' })

    expect(changes).toBe(8)
  })

  it('屏幕轨道的订阅上下线事件把 identity 交给调用方（卡片据此更新媒体状态）', () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const subscribed: string[] = []
    const unsubscribed: string[] = []
    room.onScreenSubscribed((identity) => subscribed.push(identity))
    room.onScreenUnsubscribed((identity) => unsubscribed.push(identity))

    emit('trackSubscribed', {}, makePublication('TR_s1', 'screen_share'), { identity: 'session-1' })
    emit('trackUnsubscribed', {}, makePublication('TR_s2', 'screen_share'), {
      identity: 'session-2',
    })

    expect(subscribed).toEqual(['session-1'])
    expect(unsubscribed).toEqual(['session-2'])
  })

  it('摄像头轨道的订阅上下线**不得**惊动屏幕监听器（§21：摄像头 optional，屏幕 mandatory）', () => {
    // 回归测试：房间级的 TrackSubscribed/TrackUnsubscribed 对**每一条**轨道都触发，
    // 早期实现没有按 source 过滤，于是"学生关掉摄像头"会把老师端卡片的**屏幕**订阅
    // 一起丢掉——卡片只剩"正在订阅画面…"，而业务徽章仍是 🟢（真机复现过）。
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const subscribed: string[] = []
    const unsubscribed: string[] = []
    room.onScreenSubscribed((identity) => subscribed.push(identity))
    room.onScreenUnsubscribed((identity) => unsubscribed.push(identity))

    emit('trackSubscribed', {}, makePublication('TR_cam', 'camera'), { identity: 'session-1' })
    emit('trackUnsubscribed', {}, makePublication('TR_cam', 'camera'), { identity: 'session-1' })

    expect(subscribed).toEqual([])
    expect(unsubscribed).toEqual([])
  })

  it('disconnect 会取消所有订阅并断开连接', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    joinParticipant('session-1', [screen])
    const pending = room.subscribeScreen('session-1')
    deliverTrack(screen)
    await pending

    await room.disconnect()

    expect(screen.subscribedCalls).toEqual([true, false])
    expect(lk.disconnectCalls).toEqual([true])
  })

  /* ------------------------------------------------------------------------ */
  /* §24：摄像头订阅（Phase 9）                                               */
  /* ------------------------------------------------------------------------ */

  it('§24/§52：subscribeCamera 手动订阅摄像头轨道，画质固定 LOW 并挂在 setSubscribed 之后', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const camera = makePublication('TR_camera', 'camera')
    joinParticipant('session-1', [camera])

    const pending = room.subscribeCamera('session-1')

    expect(camera.subscribedCalls).toEqual([true])
    // VideoQuality.LOW = 0：画中画是小窗，§52 的"网格优先低分辨率"在这里更极端。
    expect(camera.qualityCalls).toEqual([0])
    expect(camera.droppedQualityCalls).toEqual([])

    deliverTrack(camera)
    const subscription = await pending
    expect(subscription?.identity).toBe('session-1')

    const element = { srcObject: null } as unknown as HTMLVideoElement
    const detach = subscription?.attach(element)
    const track = camera.track as {
      attach: ReturnType<typeof vi.fn>
      detach: ReturnType<typeof vi.fn>
    }
    expect(track.attach).toHaveBeenCalledWith(element)
    detach?.()
    expect(track.detach).toHaveBeenCalledWith(element)
  })

  it('§24：同一个 participant 重复订阅摄像头不会再次 setSubscribed（十秒一轮的刷新不成风暴）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const camera = makePublication('TR_camera', 'camera')
    joinParticipant('session-1', [camera])
    const first = room.subscribeCamera('session-1')
    deliverTrack(camera)
    await first

    const second = await room.subscribeCamera('session-1')

    expect(camera.subscribedCalls).toEqual([true])
    expect(second?.identity).toBe('session-1')
  })

  it('§24：学生没有发布摄像头 → 返回 null（界面不画小窗，也不报错）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    joinParticipant('session-1', [makePublication('TR_screen', 'screen_share')])

    expect(await room.subscribeCamera('session-1')).toBeNull()
    expect(await room.subscribeCamera('nobody')).toBeNull()
  })

  it('§24：摄像头与屏幕是两条独立订阅，退掉一条不影响另一条', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    const camera = makePublication('TR_camera', 'camera')
    joinParticipant('session-1', [screen, camera])
    const screenPending = room.subscribeScreen('session-1')
    const cameraPending = room.subscribeCamera('session-1')
    deliverTrack(screen)
    deliverTrack(camera)
    await screenPending
    await cameraPending

    await room.unsubscribeCamera('session-1')

    expect(camera.subscribedCalls).toEqual([true, false])
    // 屏幕那条一次都没有被碰过（Phase 9 最要盯的性质）。
    expect(screen.subscribedCalls).toEqual([true])

    // 反过来也一样：重新订上摄像头，再退掉屏幕，摄像头不受影响。
    const cameraAgain = room.subscribeCamera('session-1')
    await cameraAgain
    await room.unsubscribeScreen('session-1')

    expect(camera.subscribedCalls).toEqual([true, false, true])
    expect(screen.subscribedCalls).toEqual([true, false])
  })

  it('§24：参与者离开时本地摄像头订阅被清掉，并把画面从元素上摘下来', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    // 事件的接线在 onParticipantsChanged 里（store 就是这么装的）。
    room.onParticipantsChanged(() => undefined)
    const camera = makePublication('TR_camera', 'camera')
    joinParticipant('session-1', [camera])
    const pending = room.subscribeCamera('session-1')
    deliverTrack(camera)
    const subscription = await pending
    const element = { srcObject: null } as unknown as HTMLVideoElement
    subscription?.attach(element)
    const track = camera.track as { detach: ReturnType<typeof vi.fn> }

    emit('participantDisconnected', { identity: 'session-1' })

    // 画面被摘下来，本地记录也没了——否则卡片右下角会停着最后一帧。
    expect(track.detach).toHaveBeenCalledWith(element)
    const again = await room.subscribeCamera('session-1')
    expect(again).not.toBe(subscription)
  })

  it('§24：摄像头轨道被取消发布 → 本地订阅记录被清掉（学生关了摄像头）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    room.onParticipantsChanged(() => undefined)
    const camera = makePublication('TR_camera', 'camera')
    joinParticipant('session-1', [camera])
    const pending = room.subscribeCamera('session-1')
    deliverTrack(camera)
    const subscription = await pending
    const element = { srcObject: null } as unknown as HTMLVideoElement
    subscription?.attach(element)
    const track = camera.track as { detach: ReturnType<typeof vi.fn> }

    emit('trackUnpublished', camera, { identity: 'session-1' })

    expect(track.detach).toHaveBeenCalledWith(element)
  })

  it('disconnect 会把摄像头订阅一起取消（一条下行都不留）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const camera = makePublication('TR_camera', 'camera')
    joinParticipant('session-1', [camera])
    const pending = room.subscribeCamera('session-1')
    deliverTrack(camera)
    await pending

    await room.disconnect()

    expect(camera.subscribedCalls).toEqual([true, false])
    expect(lk.disconnectCalls).toEqual([true])
  })

  /* ------------------------------------------------------------------------ */
  /* §32：听学生的麦克风（Phase 10）                                          */
  /* ------------------------------------------------------------------------ */

  it('§32：subscribeMicrophone 手动订阅麦克风轨道，**不设画质**，并把音频挂到 <audio>', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const mic = makePublication('TR_mic', 'microphone')
    joinParticipant('session-1', [mic])

    const pending = room.subscribeMicrophone('session-1')

    expect(mic.subscribedCalls).toEqual([true])
    // 音频没有分辨率：`setVideoQuality` 一次都不该被调用（§52 的分层只针对视频）。
    expect(mic.qualityCalls).toEqual([])
    expect(mic.droppedQualityCalls).toEqual([])
    // 顺手恢复播放：老师点开 Focus 时页面已经有用户手势，通常直接成功。
    expect(lk.startAudioCalls).toBe(1)

    deliverTrack(mic)
    const subscription = await pending
    expect(subscription?.identity).toBe('session-1')

    const element = { srcObject: null } as unknown as HTMLAudioElement
    const detach = subscription?.attach(element)
    const track = mic.track as {
      attach: ReturnType<typeof vi.fn>
      detach: ReturnType<typeof vi.fn>
    }
    expect(track.attach).toHaveBeenCalledWith(element)
    detach?.()
    expect(track.detach).toHaveBeenCalledWith(element)
  })

  it('§32：同一个 participant 不重复订阅麦克风（十秒一轮的刷新不成订阅风暴）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const mic = makePublication('TR_mic', 'microphone')
    joinParticipant('session-1', [mic])
    const first = room.subscribeMicrophone('session-1')
    deliverTrack(mic)
    await first

    const second = await room.subscribeMicrophone('session-1')

    expect(mic.subscribedCalls).toEqual([true])
    expect(second?.identity).toBe('session-1')
  })

  it('§32：学生没有发布麦克风 → 返回 null（界面说"未开启"，不报错）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    joinParticipant('session-1', [makePublication('TR_screen', 'screen_share')])

    expect(await room.subscribeMicrophone('session-1')).toBeNull()
    expect(await room.subscribeMicrophone('nobody')).toBeNull()
  })

  it('§32：三条订阅互相独立（退掉音频绝不动屏幕与摄像头）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const screen = makePublication('TR_screen', 'screen_share')
    const camera = makePublication('TR_camera', 'camera')
    const mic = makePublication('TR_mic', 'microphone')
    joinParticipant('session-1', [screen, camera, mic])
    const screenPending = room.subscribeScreen('session-1')
    const cameraPending = room.subscribeCamera('session-1')
    const micPending = room.subscribeMicrophone('session-1')
    deliverTrack(screen)
    deliverTrack(camera)
    deliverTrack(mic)
    await screenPending
    await cameraPending
    await micPending

    await room.unsubscribeMicrophone('session-1')

    expect(mic.subscribedCalls).toEqual([true, false])
    // 另外两条一次都没被碰过（Phase 10 最要盯的性质）。
    expect(screen.subscribedCalls).toEqual([true])
    expect(camera.subscribedCalls).toEqual([true])
  })

  it('§32：麦克风轨道被取消发布 → 本地订阅记录被清掉（学生关麦）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    room.onParticipantsChanged(() => undefined)
    const mic = makePublication('TR_mic', 'microphone')
    joinParticipant('session-1', [mic])
    const pending = room.subscribeMicrophone('session-1')
    deliverTrack(mic)
    const subscription = await pending
    const element = { srcObject: null } as unknown as HTMLAudioElement
    subscription?.attach(element)
    const track = mic.track as { detach: ReturnType<typeof vi.fn> }

    emit('trackUnpublished', mic, { identity: 'session-1' })

    // 声音被摘下来：否则老师的耳机里会留着一条已经死掉的音频。
    expect(track.detach).toHaveBeenCalledWith(element)
  })

  it('§32：参与者离开时本地麦克风订阅被清掉（不留"正在听"的假象）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    room.onParticipantsChanged(() => undefined)
    const mic = makePublication('TR_mic', 'microphone')
    joinParticipant('session-1', [mic])
    const pending = room.subscribeMicrophone('session-1')
    deliverTrack(mic)
    const subscription = await pending
    const element = { srcObject: null } as unknown as HTMLAudioElement
    subscription?.attach(element)
    const track = mic.track as { detach: ReturnType<typeof vi.fn> }

    emit('participantDisconnected', { identity: 'session-1' })

    expect(track.detach).toHaveBeenCalledWith(element)
    const again = await room.subscribeMicrophone('session-1')
    expect(again).not.toBe(subscription)
  })

  /* ------------------------------------------------------------------------ */
  /* §27：老师自己的麦克风（Phase 10）                                        */
  /* ------------------------------------------------------------------------ */

  it('§27：老师麦克风用 LocalAudioTrack 包装并发布到 Track.Source.Microphone（name=mic）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const track = { kind: 'audio' } as unknown as MediaStreamTrack

    await room.publishMicrophoneTrack(track)

    // 第三个参数 true = userProvidedTrack：SDK 不得自己重新采集（否则老师会被弹第二次授权框）。
    expect(lk.localAudioTrackArgs).toEqual([[track, undefined, true]])
    expect(lk.publishCalls).toHaveLength(1)
    expect(lk.publishCalls[0]?.options).toEqual({ source: 'microphone', name: 'mic' })
  })

  it('§27：unpublish 老师麦克风时不停本地轨道（释放设备是采集层的事）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    await room.publishMicrophoneTrack({ kind: 'audio' } as unknown as MediaStreamTrack)

    await room.unpublishMicrophoneTrack()

    expect(lk.unpublishCalls).toEqual([{ track: expect.anything(), stop: false }])

    // 没有发布过时是空操作（不要对 null 调 SDK）。
    await room.unpublishMicrophoneTrack()
    expect(lk.unpublishCalls).toHaveLength(1)
  })

  it('disconnect 会把麦克风订阅一起取消（一条下行都不留）', async () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const mic = makePublication('TR_mic', 'microphone')
    joinParticipant('session-1', [mic])
    const pending = room.subscribeMicrophone('session-1')
    deliverTrack(mic)
    await pending

    await room.disconnect()

    expect(mic.subscribedCalls).toEqual([true, false])
  })
})
