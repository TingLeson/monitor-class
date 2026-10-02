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
  listeners: new Map<string, Set<(...args: never[]) => void>>(),
  instances: [] as { remoteParticipants: Map<string, unknown> }[],
}))

/** publication 替身：记录 setSubscribed / setEnabled 的调用。 */
interface FakePublication {
  trackSid: string
  source: string
  isSubscribed: boolean
  track?: unknown
  subscribedCalls: boolean[]
}

function makePublication(sid: string, source: string): FakePublication {
  const publication: FakePublication = {
    trackSid: sid,
    source,
    isSubscribed: false,
    subscribedCalls: [],
  }
  return Object.assign(publication, {
    setSubscribed(value: boolean): void {
      publication.subscribedCalls.push(value)
      publication.isSubscribed = value
    },
    setEnabled(): void {
      // Phase 6 不使用它（那是 Phase 7 的按可见性暂停下行）。
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
  class Room {
    readonly remoteParticipants = new Map<string, unknown>()

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

  it('参与者事件驱动订阅协调：加入/离开、屏幕轨道的发布与取消都会通知', () => {
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
    // 摄像头/麦克风事件与本 Phase 无关（§54）：不通知，免得白白重建订阅。
    emit('trackPublished', makePublication('TR_camera', 'camera'), {
      identity: 'session-2',
      trackPublications: new Map([['TR_camera', makePublication('TR_camera', 'camera')]]),
    })

    expect(changes).toBe(4)
  })

  it('订阅上下线事件把 identity 交给调用方（卡片据此更新媒体状态）', () => {
    const room = createLiveKitMonitorRoom(CREDENTIALS)
    const subscribed: string[] = []
    const unsubscribed: string[] = []
    room.onScreenSubscribed((identity) => subscribed.push(identity))
    room.onScreenUnsubscribed((identity) => unsubscribed.push(identity))

    emit('trackSubscribed', {}, {}, { identity: 'session-1' })
    emit('trackUnsubscribed', {}, {}, { identity: 'session-2' })

    expect(subscribed).toEqual(['session-1'])
    expect(unsubscribed).toEqual(['session-2'])
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
})
