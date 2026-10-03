import type { RealtimeEvent } from '@classwatch/shared-types'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RealtimeSocket, type RealtimeSocketLike } from '../realtime-socket'

/**
 * 共享实时通道客户端测试（§47 / §52 / §64）。
 *
 * 这一份测试盯的是**通道本身**能不能被信任，而不是任何业务：
 * 连接建立、心跳假死、指数退避、不可信报文、关闭清理。
 * 业务路由（哪个事件交给哪个 store）在 app 层的测试里覆盖。
 *
 * 时间与 socket 都注入：`vi.useFakeTimers()` + 假 `RealtimeSocketLike`，
 * 于是"退避是不是真的按 2 的幂增长""心跳超时会不会主动断开"这类性质
 * 可以逐毫秒钉死，而不是靠 sleep 去撞。
 */

/** 一个受控的假 WebSocket：不连网络，由测试决定何时 open / message / close。 */
class FakeWebSocket implements RealtimeSocketLike {
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null

  /** 客户端发出去的所有文本（断言 PING 报文用）。 */
  readonly sent: string[] = []
  closeCalls = 0

  constructor(readonly url: string) {}

  send(data: string): void {
    this.sent.push(data)
  }

  close(): void {
    this.closeCalls += 1
  }

  /** 握手成功。 */
  open(): void {
    this.onopen?.(new Event('open'))
  }

  /** 服务端推来一条 JSON（对象会被序列化，字符串原样发出）。 */
  message(payload: unknown): void {
    this.messageRaw(typeof payload === 'string' ? payload : JSON.stringify(payload))
  }

  /** 任意类型的原始帧（用于覆盖"非文本报文"）。 */
  messageRaw(data: unknown): void {
    this.onmessage?.({ data } as MessageEvent)
  }

  /** 服务端主动断开（也可能是握手被拒：两者在浏览器里都表现为 close）。 */
  serverClose(closeCode = 1006): void {
    // 浏览器把服务端的 close code 放在 CloseEvent.code 上；默认 1006 = 异常关闭。
    this.onclose?.({ code: closeCode } as CloseEvent)
  }
}

interface Harness {
  readonly sockets: FakeWebSocket[]
  current(): FakeWebSocket
  /** 创建过的连接数（重连会 +1）。 */
  count(): number
}

const logger = { warn: vi.fn() }

function makeHarness(): Harness {
  const sockets: FakeWebSocket[] = []
  return {
    sockets,
    current(): FakeWebSocket {
      const last = sockets[sockets.length - 1]
      if (!last) throw new Error('还没有创建任何连接')
      return last
    },
    count: () => sockets.length,
  }
}

const EVENT: RealtimeEvent<'ROOM_CLOSED'> = {
  type: 'ROOM_CLOSED',
  at: '2026-10-03T10:00:00Z',
  data: { classroomId: 'room-1', runId: 'run-1', closedAt: '2026-10-03T10:00:00Z' },
}

/** 造一个客户端；默认参数刻意很小，让测试用毫秒级假时钟推进。 */
function makeSocket(
  harness: Harness,
  overrides: {
    onEvent?: (event: RealtimeEvent) => void
    onStateChange?: (state: string) => void
    heartbeatIntervalMs?: number
    heartbeatTimeoutMs?: number
    reconnectBaseMs?: number
    reconnectMaxMs?: number
    reconnectJitterRatio?: number
    handshakeFailureLimit?: number
    random?: () => number
  } = {},
): { socket: RealtimeSocket; states: string[]; events: RealtimeEvent[] } {
  const states: string[] = []
  const events: RealtimeEvent[] = []
  const socket = new RealtimeSocket({
    url: 'ws://localhost:5173/ws/student',
    onEvent: (event) => {
      events.push(event)
      overrides.onEvent?.(event)
    },
    onStateChange: (state) => {
      states.push(state)
      overrides.onStateChange?.(state)
    },
    socketFactory: (url) => {
      const created = new FakeWebSocket(url)
      harness.sockets.push(created)
      return created
    },
    heartbeatIntervalMs: overrides.heartbeatIntervalMs ?? 1_000,
    heartbeatTimeoutMs: overrides.heartbeatTimeoutMs ?? 500,
    reconnectBaseMs: overrides.reconnectBaseMs ?? 100,
    reconnectMaxMs: overrides.reconnectMaxMs ?? 1_600,
    reconnectJitterRatio: overrides.reconnectJitterRatio ?? 0,
    handshakeFailureLimit: overrides.handshakeFailureLimit ?? 3,
    random: overrides.random ?? (() => 0),
    logger,
  })
  return { socket, states, events }
}

describe('实时通道客户端', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    logger.warn.mockClear()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('connect：地址原样交给工厂（同源、不带任何凭据），状态 connecting → open', () => {
    const harness = makeHarness()
    const { socket, states } = makeSocket(harness)

    socket.connect()

    expect(harness.current().url).toBe('ws://localhost:5173/ws/student')
    expect(socket.state).toBe('connecting')

    harness.current().open()

    expect(socket.state).toBe('open')
    expect(states).toEqual(['connecting', 'open'])
  })

  it('connect 幂等：已经连着的时候不会建第二条连接', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)

    socket.connect()
    socket.connect()
    harness.current().open()
    socket.connect()

    expect(harness.count()).toBe(1)
  })

  it('合法事件交给 onEvent（判别式与 data 都保持原样）', () => {
    const harness = makeHarness()
    const { socket, events } = makeSocket(harness)
    socket.connect()
    harness.current().open()

    harness.current().message(EVENT)

    expect(events).toEqual([EVENT])
  })

  it('不可信报文一律忽略并计数：未知类型 / 缺字段 / 非 JSON / 非文本', () => {
    const harness = makeHarness()
    const { socket, events } = makeSocket(harness)
    socket.connect()
    harness.current().open()

    harness.current().message({ type: 'SOMETHING_ELSE', at: EVENT.at, data: {} })
    harness.current().message({ type: 'ROOM_CLOSED', at: EVENT.at, data: { classroomId: 'x' } })
    harness.current().message({ type: 'ROOM_CLOSED', data: EVENT.data })
    harness.current().message('{ not json')
    harness.current().messageRaw(42)

    expect(events).toEqual([])
    expect(socket.droppedMessages).toBe(5)
    // 通道本身没有任何损伤：下一条合法事件照样送达。
    harness.current().message(EVENT)
    expect(events).toEqual([EVENT])
    expect(socket.state).toBe('open')
  })

  it('STUDENT_OFFLINE 的 reason 必须是枚举值（拼错的 reason 不当成离线事件）', () => {
    const harness = makeHarness()
    const { socket, events } = makeSocket(harness)
    socket.connect()
    harness.current().open()

    harness.current().message({
      type: 'STUDENT_OFFLINE',
      at: EVENT.at,
      data: { studentId: 's-1', sessionId: 'sess-1', reason: 'BYE' },
    })
    expect(events).toEqual([])

    harness.current().message({
      type: 'STUDENT_OFFLINE',
      at: EVENT.at,
      data: { studentId: 's-1', sessionId: 'sess-1', reason: 'LEFT' },
    })
    expect(events).toHaveLength(1)
  })

  it('学生端的 SCREEN_LOST（只有 sessionId，§26）是合法报文', () => {
    const harness = makeHarness()
    const { socket, events } = makeSocket(harness)
    socket.connect()
    harness.current().open()

    harness.current().message({
      type: 'SCREEN_LOST',
      at: EVENT.at,
      data: { sessionId: 'session-1' },
    })

    expect(events).toHaveLength(1)
  })

  it('PONG 不进业务回调，但能续命心跳（否则每次心跳都会误判断线）', () => {
    const harness = makeHarness()
    const { socket, events } = makeSocket(harness)
    socket.connect()
    harness.current().open()

    // 打开即发一次 PING（尽早发现假死，而不是等满一个心跳周期）。
    expect(harness.current().sent).toEqual(['{"type":"PING"}'])

    harness.current().message({ type: 'PONG' })
    // 超时窗口过去但没有重连：PONG 已经清了计时器。
    vi.advanceTimersByTime(600)

    expect(events).toEqual([])
    expect(socket.state).toBe('open')
    expect(harness.count()).toBe(1)
  })

  it('心跳超时：主动断开判定假死并重连（TCP 假死不能让 UI 一直显示"已连接"）', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)
    socket.connect()
    const first = harness.current()
    first.open()

    vi.advanceTimersByTime(500)

    expect(first.closeCalls).toBe(1)
    expect(socket.state).toBe('connecting')

    vi.advanceTimersByTime(100)
    expect(harness.count()).toBe(2)
  })

  it('服务端关闭：按 base × 2^n 退避重连（100 → 200 → 400）', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)
    socket.connect()
    harness.current().open()

    harness.current().serverClose()

    vi.advanceTimersByTime(99)
    expect(harness.count()).toBe(1)
    vi.advanceTimersByTime(1)
    expect(harness.count()).toBe(2)

    harness.current().serverClose()
    vi.advanceTimersByTime(199)
    expect(harness.count()).toBe(2)
    vi.advanceTimersByTime(1)
    expect(harness.count()).toBe(3)

    harness.current().serverClose()
    vi.advanceTimersByTime(399)
    expect(harness.count()).toBe(3)
    vi.advanceTimersByTime(1)
    expect(harness.count()).toBe(4)

    expect(socket.state).toBe('connecting')
  })

  it('退避有上限（reconnectMaxMs）：不会无限翻倍', () => {
    const harness = makeHarness()
    // 关掉"握手被拒"的止损，这里只观察退避曲线本身。
    const { socket } = makeSocket(harness, {
      reconnectBaseMs: 1_000,
      reconnectMaxMs: 2_000,
      handshakeFailureLimit: 10,
    })
    socket.connect()
    harness.current().serverClose() // 第 1 次失败 → 1000ms

    vi.advanceTimersByTime(1_000)
    harness.current().serverClose() // 第 2 次失败 → min(2000, 2000)
    vi.advanceTimersByTime(2_000)
    harness.current().serverClose() // 第 3 次失败 → 仍然是 2000，而不是 4000
    vi.advanceTimersByTime(1_999)
    expect(harness.count()).toBe(3)
    vi.advanceTimersByTime(1)
    expect(harness.count()).toBe(4)
    // 上限对第 4 次失败同样生效：等 2000ms，而不是翻到 4000ms。
    harness.current().serverClose()
    vi.advanceTimersByTime(1_999)
    expect(harness.count()).toBe(4)
    vi.advanceTimersByTime(1)
    expect(harness.count()).toBe(5)
  })

  it('抖动只加不减，且总等待不超过上限（避免整班同时重连）', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness, {
      reconnectBaseMs: 100,
      reconnectMaxMs: 100,
      reconnectJitterRatio: 0.5,
      random: () => 1,
    })
    socket.connect()
    harness.current().serverClose()

    // 100 + 50% 抖动 = 150，但上限是 100，所以仍然只等 100ms。
    vi.advanceTimersByTime(100)
    expect(harness.count()).toBe(2)
  })

  it('成功打开一次之后退避计数归零（下一次断线从 base 重新开始）', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)
    socket.connect()
    harness.current().serverClose() // 1 → 100ms
    vi.advanceTimersByTime(100)
    harness.current().serverClose() // 2 → 200ms
    vi.advanceTimersByTime(200)
    harness.current().open() // 连上了：计数归零

    harness.current().serverClose()
    vi.advanceTimersByTime(100)

    expect(harness.count()).toBe(4)
  })

  it('从未打开过 + 连续多次失败：判定握手被拒，停止无脑重连（浏览器拿不到 401 状态码）', () => {
    const harness = makeHarness()
    const { socket, states } = makeSocket(harness, { handshakeFailureLimit: 3 })
    socket.connect()

    harness.current().serverClose() // 1
    vi.advanceTimersByTime(100)
    harness.current().serverClose() // 2
    vi.advanceTimersByTime(200)
    harness.current().serverClose() // 3 → 放弃

    expect(socket.authFailed).toBe(true)
    expect(socket.state).toBe('closed')

    vi.advanceTimersByTime(60_000)
    expect(harness.count()).toBe(3)
    expect(states[states.length - 1]).toBe('closed')
  })

  it('服务端用 close code 4401 拒绝：立即判定鉴权失败，不做任何重连', () => {
    const harness = makeHarness()
    const { socket, states } = makeSocket(harness)
    socket.connect()

    // 后端 §74 的实现：握手升级成功后立刻用 4401 关闭（没有有效会话 / 账号停用）。
    harness.current().open()
    harness.current().serverClose(4401)

    expect(socket.authFailed).toBe(true)
    expect(socket.state).toBe('closed')

    // 关键：不能退避重连——重连一万次结果一样，只会打后端。
    vi.advanceTimersByTime(60_000)
    expect(harness.count()).toBe(1)
    expect(states[states.length - 1]).toBe('closed')
  })

  it('服务端用 close code 4403 拒绝（会话属于另一个入口）：同样立即停止', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)
    socket.connect()

    harness.current().open()
    harness.current().serverClose(4403)

    expect(socket.authFailed).toBe(true)
    vi.advanceTimersByTime(60_000)
    expect(harness.count()).toBe(1)
  })

  it('普通异常关闭（1006）仍然按退避重连：断线不该被当成鉴权失败', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)
    socket.connect()

    harness.current().open()
    harness.current().serverClose(1006)

    expect(socket.authFailed).toBe(false)
    vi.advanceTimersByTime(100)
    expect(harness.count()).toBe(2)
  })

  it('close()：清理定时器、关闭底层连接、不再重连', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)
    socket.connect()
    harness.current().open()
    const current = harness.current()

    socket.close()

    expect(current.closeCalls).toBe(1)
    expect(socket.state).toBe('closed')
    expect(vi.getTimerCount()).toBe(0)

    // 真实浏览器里 close() 之后仍会异步回调 onclose：不能因此又拉起一条连接。
    current.serverClose()
    vi.advanceTimersByTime(60_000)
    expect(harness.count()).toBe(1)
    expect(socket.state).toBe('closed')
  })

  it('onEvent 抛错不影响通道：记一句日志，后续消息照常送达', () => {
    const harness = makeHarness()
    const events: RealtimeEvent[] = []
    const { socket } = makeSocket(harness, {
      onEvent: (event) => {
        events.push(event)
        throw new Error('订阅方炸了')
      },
    })
    socket.connect()
    harness.current().open()

    harness.current().message(EVENT)
    harness.current().message({ ...EVENT, at: '2026-10-03T10:01:00Z' })

    expect(events).toHaveLength(2)
    expect(socket.state).toBe('open')
    expect(logger.warn).toHaveBeenCalled()
  })

  it('§44：日志与工厂参数里都不出现凭据（地址是同源路径，本来就没有 token）', () => {
    const harness = makeHarness()
    const { socket } = makeSocket(harness)
    socket.connect()
    harness.current().open()
    harness.current().message({ type: 'UNKNOWN', at: EVENT.at, data: {} })
    harness.current().serverClose()
    vi.advanceTimersByTime(10_000)

    expect(harness.current().url).not.toMatch(/token|cookie|auth=/i)
    for (const call of logger.warn.mock.calls) {
      const line = String(call[0])
      expect(line).not.toMatch(/token|cookie|wss?:\/\//i)
    }
  })
})
