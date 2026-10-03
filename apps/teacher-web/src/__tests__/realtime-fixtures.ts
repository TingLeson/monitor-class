import type { RealtimeSocketFactory, RealtimeSocketLike } from '@classwatch/api-client'
import type { RealtimeEvent, StudentOfflineReason } from '@classwatch/shared-types'
import { afterEach } from 'vitest'
import { setRealtimeSocketFactory } from '../lib/realtime-channel.ts'
import { resetRealtimeChannel } from '../stores/realtime.ts'

/**
 * 老师端实时通道测试替身（§47 / §64 Frontend Test）。
 *
 * 与学生端的替身是同一个模式（假 socket + 假工厂注入），差别只在**事件夹具**：
 * 老师收到的是 `{studentId, sessionId, ...}` 那一版载荷（§47 的收件人表按角色
 * 给不同形状），学生端收到的是不含 studentId 的那一版。
 * 把这件事写进夹具，测试里就不会出现"用一个学生形状的事件去测老师端"的假通过。
 */

export class FakeRealtimeSocket implements RealtimeSocketLike {
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null

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
  open(): void
  emit(event: RealtimeEvent): void
  emitRaw(text: string): void
  close(): void
}

/** 装上假工厂；每个测试结束后由本文件的 afterEach 还原。 */
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
/* 事件夹具（§47 的冻结载荷，老师视角）                                        */
/* -------------------------------------------------------------------------- */

const AT = '2026-10-03T10:00:00Z'

/** `ROOM_CLOSED`：学生与 owner 老师都会收到（§49）。 */
export function makeRoomClosedEvent(
  overrides: Partial<{ classroomId: string; runId: string; closedAt: string }> = {},
): RealtimeEvent<'ROOM_CLOSED'> {
  return {
    type: 'ROOM_CLOSED',
    at: AT,
    data: { classroomId: 'room-1', runId: 'run-1', closedAt: AT, ...overrides },
  }
}

/** `STUDENT_ONLINE`：老师收到学生上线（§12 的 CONNECTING → ONLINE）。 */
export function makeStudentOnlineEvent(
  overrides: Partial<{ studentId: string; displayName: string; sessionId: string }> = {},
): RealtimeEvent<'STUDENT_ONLINE'> {
  return {
    type: 'STUDENT_ONLINE',
    at: AT,
    data: { studentId: 'student-1', displayName: '张三', sessionId: 'session-1', ...overrides },
  }
}

/** `STUDENT_OFFLINE`：reason 是三种之一（§12）。 */
export function makeStudentOfflineEvent(
  overrides: Partial<{ studentId: string; sessionId: string; reason: StudentOfflineReason }> = {},
): RealtimeEvent<'STUDENT_OFFLINE'> {
  return {
    type: 'STUDENT_OFFLINE',
    at: AT,
    data: { studentId: 'student-1', sessionId: 'session-1', reason: 'LEFT', ...overrides },
  }
}

/** `SCREEN_LOST`：**老师**收到的形状带 studentId（§47）。 */
export function makeScreenLostEvent(
  overrides: Partial<{ studentId: string; sessionId: string }> = {},
): RealtimeEvent<'SCREEN_LOST'> {
  return {
    type: 'SCREEN_LOST',
    at: AT,
    data: { studentId: 'student-1', sessionId: 'session-1', ...overrides },
  }
}

/** `SCREEN_RESTORED`：同上。 */
export function makeScreenRestoredEvent(
  overrides: Partial<{ studentId: string; sessionId: string }> = {},
): RealtimeEvent<'SCREEN_RESTORED'> {
  return {
    type: 'SCREEN_RESTORED',
    at: AT,
    data: { studentId: 'student-1', sessionId: 'session-1', ...overrides },
  }
}

/** `CAMERA_CHANGED` / `MIC_CHANGED`（Phase 9/10 才会有，§24/§25）。 */
export function makeCameraChangedEvent(
  overrides: Partial<{ studentId: string; sessionId: string; active: boolean }> = {},
): RealtimeEvent<'CAMERA_CHANGED'> {
  return {
    type: 'CAMERA_CHANGED',
    at: AT,
    data: { studentId: 'student-1', sessionId: 'session-1', active: true, ...overrides },
  }
}

afterEach(() => {
  // 连接引用活在 store 模块的模块作用域里（HMR 需要），必须显式清掉。
  resetRealtimeChannel()
  setRealtimeSocketFactory(null)
})
