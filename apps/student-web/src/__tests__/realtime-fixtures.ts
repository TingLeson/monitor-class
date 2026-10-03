import type { RealtimeSocketFactory, RealtimeSocketLike } from '@classwatch/api-client'
import type { RealtimeEvent } from '@classwatch/shared-types'
import { afterEach } from 'vitest'
import { setRealtimeSocketFactory } from '../lib/realtime-channel.ts'
import { resetRealtimeChannel } from '../stores/realtime.ts'

/**
 * 学生端实时通道测试替身（§47 / §64 Frontend Test）。
 *
 * 替身挂在 `realtime-channel.ts` 的工厂注入点上，而不是 `vi.mock('@classwatch/api-client')`：
 * 我们要断言的是**业务路由**（哪条事件改了哪个 store），而连接维持、心跳、退避
 * 那些真正的复杂度由 `packages/api-client` 自己的测试覆盖。这里只保留"什么时候
 * open、收到什么报文、什么时候断"这三件事的控制权。
 *
 * 报文走 `emit()` 而不是直接调 store 的方法：这样测试覆盖的是完整链路
 * （JSON → isRealtimeEvent 守卫 → store 路由），而不是一个"测试专用入口"。
 */

export class FakeRealtimeSocket implements RealtimeSocketLike {
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null

  /** 客户端发出的文本（心跳 PING 也会出现在这里）。 */
  readonly sent: string[] = []
  closeCalls = 0

  constructor(readonly url: string) {}

  send(data: string): void {
    this.sent.push(data)
  }

  close(): void {
    this.closeCalls += 1
  }

  open(): void {
    this.onopen?.(new Event('open'))
  }

  message(data: unknown): void {
    this.onmessage?.({ data } as MessageEvent)
  }

  serverClose(): void {
    this.onclose?.(new Event('close') as CloseEvent)
  }
}

export interface RealtimeHarness {
  readonly sockets: FakeRealtimeSocket[]
  current(): FakeRealtimeSocket
  /** 握手成功（连接状态变 open）。 */
  open(): void
  /** 推一条业务事件（对象会被序列化成 JSON 文本）。 */
  emit(event: RealtimeEvent): void
  /** 推一条任意原始报文（覆盖不可信输入）。 */
  emitRaw(text: string): void
  /** 服务端断开（也可能是握手被拒，浏览器里两者形状相同）。 */
  close(): void
}

/**
 * 装上假工厂（每个测试结束后由本文件的 afterEach 还原）。
 *
 * 工厂在 store 调 `start()` 时被调用一次；重连会创建新实例，因此这里按
 * "当前最后一条"取用，与真实客户端的行为一致。
 */
export function installFakeRealtimeSocket(): RealtimeHarness {
  const sockets: FakeRealtimeSocket[] = []
  const factory: RealtimeSocketFactory = (url) => {
    const socket = new FakeRealtimeSocket(url)
    sockets.push(socket)
    return socket
  }
  setRealtimeSocketFactory(factory)

  const current = (): FakeRealtimeSocket => {
    const last = sockets[sockets.length - 1]
    if (!last) throw new Error('实时通道还没有被创建（store.start() 调过了吗？）')
    return last
  }

  return {
    sockets,
    current,
    open: () => current().open(),
    emit: (event) => current().message(JSON.stringify(event)),
    emitRaw: (text) => current().message(text),
    close: () => current().serverClose(),
  }
}

/* -------------------------------------------------------------------------- */
/* 事件夹具（§47 的冻结载荷）                                                  */
/* -------------------------------------------------------------------------- */

const AT = '2026-10-03T10:00:00Z'

/** `ROOM_OPENED`：老师开课（§48），只推给学生自己被授权的课堂。 */
export function makeRoomOpenedEvent(
  overrides: Partial<{
    classroomId: string
    classroomName: string
    runId: string
    openedAt: string
  }> = {},
): RealtimeEvent<'ROOM_OPENED'> {
  return {
    type: 'ROOM_OPENED',
    at: AT,
    data: {
      classroomId: 'room-1',
      classroomName: 'C++ 算法训练',
      runId: 'run-2',
      openedAt: AT,
      ...overrides,
    },
  }
}

/** `ROOM_CLOSED`：老师关课（§49），学生与 owner 老师都会收到。 */
export function makeRoomClosedEvent(
  overrides: Partial<{ classroomId: string; runId: string; closedAt: string }> = {},
): RealtimeEvent<'ROOM_CLOSED'> {
  return {
    type: 'ROOM_CLOSED',
    at: AT,
    data: { classroomId: 'room-1', runId: 'run-2', closedAt: AT, ...overrides },
  }
}

/** `SCREEN_LOST`：**学生本人**收到的形状只有 sessionId（§26）。 */
export function makeScreenLostEvent(sessionId = 'session-1'): RealtimeEvent<'SCREEN_LOST'> {
  return { type: 'SCREEN_LOST', at: AT, data: { sessionId } }
}

/** `SCREEN_RESTORED`：同上。 */
export function makeScreenRestoredEvent(sessionId = 'session-1'): RealtimeEvent<'SCREEN_RESTORED'> {
  return { type: 'SCREEN_RESTORED', at: AT, data: { sessionId } }
}

afterEach(() => {
  // 连接引用活在 store 模块的模块作用域里（HMR 需要），必须显式清掉，
  // 否则下一个用例 start() 会因为"已经有一条连接"直接返回。
  resetRealtimeChannel()
  setRealtimeSocketFactory(null)
})
