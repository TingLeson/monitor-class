import type { MonitorStudent } from '@classwatch/shared-types'
import { afterEach } from 'vitest'
import {
  resetMonitorRoomFactory,
  setMonitorRoomFactory,
  type MediaCredentials,
  type MediaDisconnectReason,
  type MediaRemoteParticipant,
  type MonitorRoom,
  type ScreenSubscription,
} from '../lib/media/media-room.ts'

/**
 * 老师端媒体测试替身（§51 / §52）。
 *
 * 替身挂在 `media-room.ts` 的工厂注入点上（而不是 `vi.mock('livekit-client')`），
 * 因为这一层要断言的是**业务性质**：只订阅 `screen.active` 的学生、
 * 同一个 participant 不重复订阅、取消不可见的订阅。真实 SDK 的调用形状
 * （`autoSubscribe=false`、`setSubscribed(true)`）由 `livekit-room.spec.ts` 单独钉住。
 */

export interface FakeMonitorRoom {
  readonly room: MonitorRoom
  readonly connectCalls: number
  readonly disconnectCalls: number
  /**
   * 每一次 `subscribeScreen` 调用（**不去重**）。
   *
   * 替身刻意不自己实现幂等：如果它顺手去重了，store 里那道
   * "不重复订阅"的防线就永远不会被测出来——而这正是本 Phase 最要盯的性质之一。
   */
  readonly subscribeCalls: string[]
  readonly unsubscribeCalls: string[]
  /** 被 attach 过的 `<video>`（断言"画面真的挂上去了"）。 */
  readonly attachedElements: HTMLVideoElement[]
  /** 媒体层的参与者事实（业务状态不在这里，见 §51）。 */
  participants: MediaRemoteParticipant[]
  /** false 表示"业务说该有画面，但媒体里还没有这条轨道"（返回 null）。 */
  screenAvailable: boolean
  /**
   * 让 subscribeScreen 抛错（订阅失败 → 卡片显示失败 + 可重试）。
   *
   * 与 connectError 一样读写整个 harness 共享的开关：房间是懒创建的，
   * 测试往往需要在房间出现之前就把失败打开。
   */
  subscribeError: Error | null
  emitParticipantsChanged(): void
  emitScreenSubscribed(identity: string): void
  emitScreenUnsubscribed(identity: string): void
  emitDisconnected(reason?: MediaDisconnectReason): void
}

export interface MonitorRoomHarness {
  readonly rooms: FakeMonitorRoom[]
  readonly credentials: MediaCredentials[]
  flags: {
    connectError: Error | null
    subscribeError: Error | null
  }
  current(): FakeMonitorRoom
}

/** 装上假工厂（每个测试结束后由本文件的 afterEach 还原）。 */
export function installFakeMonitorRoom(
  options: { connectError?: Error; participants?: string[] } = {},
): MonitorRoomHarness {
  const rooms: FakeMonitorRoom[] = []
  const credentials: MediaCredentials[] = []
  const flags = {
    connectError: options.connectError ?? null,
    subscribeError: null as Error | null,
  }
  /**
   * 媒体层里"真的有谁"。
   *
   * 默认是夹具的默认 sessionId（`session-1`）：绝大多数用例只想验证"订阅能成功"。
   * 传数组即进入**严格模式**（房间里的身份以它为准），用于验证"业务状态领先于
   * 媒体状态"那条真实路径——DTO 说学生在共享，而媒体里人已经走了。
   */
  const participants = options.participants ?? ['session-1']

  setMonitorRoomFactory((next) => {
    credentials.push(next)
    const fake = makeFakeMonitorRoom(flags)
    fake.participants = participants.map((identity) => ({ identity, hasScreen: true }))
    rooms.push(fake)
    return Promise.resolve(fake.room)
  })

  return {
    rooms,
    credentials,
    flags,
    current(): FakeMonitorRoom {
      const last = rooms[rooms.length - 1]
      if (!last) throw new Error('还没有创建任何假房间')
      return last
    },
  }
}

function makeFakeMonitorRoom(flags: {
  connectError: Error | null
  subscribeError: Error | null
}): FakeMonitorRoom {
  const participantsChanged: (() => void)[] = []
  const subscribed: ((identity: string) => void)[] = []
  const unsubscribed: ((identity: string) => void)[] = []
  const disconnected: ((reason: MediaDisconnectReason) => void)[] = []

  const subscribeCalls: string[] = []
  const unsubscribeCalls: string[] = []
  const attachedElements: HTMLVideoElement[] = []
  let connectCalls = 0
  let disconnectCalls = 0

  const room: MonitorRoom = {
    connect(): Promise<void> {
      connectCalls += 1
      if (flags.connectError) return Promise.reject(flags.connectError)
      return Promise.resolve()
    },
    disconnect(): Promise<void> {
      disconnectCalls += 1
      return Promise.resolve()
    },
    participants: () => [...self.participants],
    subscribeScreen(identity: string): Promise<ScreenSubscription | null> {
      subscribeCalls.push(identity)
      if (flags.subscribeError) return Promise.reject(flags.subscribeError)
      // 与真实适配层一致：房间里根本没有这个参与者时返回 null
      // （业务状态可能领先于媒体状态，此时卡片显示"正在订阅画面…"）。
      if (!self.participants.some((participant) => participant.identity === identity)) {
        return Promise.resolve(null)
      }
      if (!self.screenAvailable) return Promise.resolve(null)
      const subscription: ScreenSubscription = {
        identity,
        attach(element: HTMLVideoElement): () => void {
          attachedElements.push(element)
          return () => {
            const index = attachedElements.indexOf(element)
            if (index >= 0) attachedElements.splice(index, 1)
          }
        },
      }
      return Promise.resolve(subscription)
    },
    unsubscribeScreen(identity: string): Promise<void> {
      unsubscribeCalls.push(identity)
      return Promise.resolve()
    },
    onParticipantsChanged(listener) {
      participantsChanged.push(listener)
      return () => removeFrom(participantsChanged, listener)
    },
    onScreenSubscribed(listener) {
      subscribed.push(listener)
      return () => removeFrom(subscribed, listener)
    },
    onScreenUnsubscribed(listener) {
      unsubscribed.push(listener)
      return () => removeFrom(unsubscribed, listener)
    },
    onDisconnected(listener) {
      disconnected.push(listener)
      return () => removeFrom(disconnected, listener)
    },
  }

  const self: FakeMonitorRoom = {
    room,
    get connectCalls() {
      return connectCalls
    },
    get disconnectCalls() {
      return disconnectCalls
    },
    subscribeCalls,
    unsubscribeCalls,
    attachedElements,
    participants: [],
    screenAvailable: true,
    get subscribeError() {
      return flags.subscribeError
    },
    set subscribeError(next: Error | null) {
      flags.subscribeError = next
    },
    emitParticipantsChanged() {
      for (const listener of [...participantsChanged]) listener()
    },
    emitScreenSubscribed(identity) {
      for (const listener of [...subscribed]) listener(identity)
    },
    emitScreenUnsubscribed(identity) {
      for (const listener of [...unsubscribed]) listener(identity)
    },
    emitDisconnected(reason: MediaDisconnectReason = null) {
      for (const listener of [...disconnected]) listener(reason)
    },
  }
  return self
}

function removeFrom<T>(list: T[], item: T): void {
  const index = list.indexOf(item)
  if (index >= 0) list.splice(index, 1)
}

afterEach(() => {
  resetMonitorRoomFactory()
})

/* -------------------------------------------------------------------------- */
/* Monitor DTO 夹具（§51）                                                     */
/* -------------------------------------------------------------------------- */

/**
 * 一个"正在上课、屏幕正常"的学生。
 *
 * 默认值刻意选成**最完整**的在线形态：任何"未连接 / 屏幕中断"的断言都必须显式
 * 覆盖 sessionStatus 或 screen.active——这样就不会出现"夹具默认值让测试误判通过"。
 */
export function makeMonitorStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return {
    studentId: 'student-1',
    displayName: '张三',
    sessionId: 'session-1',
    sessionStatus: 'ONLINE',
    screen: { active: true },
    camera: { active: false },
    microphone: { active: false },
    connection: 'GOOD',
    joinedAt: '2026-10-03T10:00:00Z',
    lastEventAt: '2026-10-03T10:05:00Z',
    ...overrides,
  }
}

/** 屏幕中断的学生（§22 在老师端的形态）。 */
export function makeScreenLostStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return makeMonitorStudent({
    studentId: 'student-lost',
    displayName: '李四',
    sessionId: 'session-lost',
    sessionStatus: 'SCREEN_LOST',
    screen: { active: false },
    ...overrides,
  })
}

/** 从未进入课堂的学生（名单里有他，但没有 Active Session）。 */
export function makeOfflineStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return makeMonitorStudent({
    studentId: 'student-offline',
    displayName: '王五',
    sessionId: null,
    sessionStatus: 'DISCONNECTED',
    screen: { active: false },
    connection: 'UNKNOWN',
    joinedAt: null,
    lastEventAt: null,
    ...overrides,
  })
}
