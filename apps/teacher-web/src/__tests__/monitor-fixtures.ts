import type { MonitorStudent } from '@classwatch/shared-types'
import { afterEach } from 'vitest'
import {
  resetMonitorRoomFactory,
  setMonitorRoomFactory,
  type CameraSubscription,
  type MediaCredentials,
  type MediaDisconnectReason,
  type MediaRemoteParticipant,
  type MicrophoneSubscription,
  type MonitorRoom,
  type ScreenQuality,
  type ScreenSubscription,
} from '../lib/media/media-room.ts'
import {
  resetTileVisibilityFactory,
  setTileVisibilityFactory,
  type TileVisibilityHandle,
} from '../lib/tile-visibility.ts'

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
  /**
   * 每次订阅请求带的画质档（§52 的"网格低 / Focus 高"）。
   *
   * 与 subscribeCalls 一一对应；分开记录是为了让断言能直接写成
   * `qualitiesFor('session-1') === ['low', 'high']` 这样的一行。
   */
  readonly subscribeQualities: ScreenQuality[]
  /** 每条订阅上的 setQuality 调用（切换 Focus 时必须走这条路，而不是重新订阅）。 */
  readonly qualityChanges: { identity: string; quality: ScreenQuality }[]
  readonly unsubscribeCalls: string[]
  /**
   * 摄像头的订阅调用（§24，**不去重**，理由同 subscribeCalls）。
   *
   * 与屏幕那几个数组分开：Phase 9 最要盯的性质就是"两条轨道互不影响"，
   * 共用一个数组会让"取消摄像头订阅顺手把屏幕也退了"这种 bug 测不出来。
   */
  readonly subscribeCameraCalls: string[]
  readonly unsubscribeCameraCalls: string[]
  /** 摄像头被 attach 过的 `<video>`（断言画中画的画面真的挂上去了）。 */
  readonly cameraAttachedElements: HTMLVideoElement[]
  /**
   * 麦克风的订阅调用（§32，同样**不去重**）。
   *
   * 与摄像头那几个数组分开：本 Phase 最要盯的性质是"同一时刻最多一路音频"，
   * 共用一个数组会让"Focus 切换时旧的那路没退掉"这种 bug 测不出来。
   */
  readonly subscribeMicrophoneCalls: string[]
  readonly unsubscribeMicrophoneCalls: string[]
  /** 麦克风被 attach 过的 `<audio>`（断言"正在听"这句话背后真的有声音）。 */
  readonly microphoneAttachedElements: HTMLAudioElement[]
  /** 老师自己发布过的麦克风轨道（§27/§31）。 */
  readonly publishedMicrophoneTracks: MediaStreamTrack[]
  readonly unpublishMicrophoneCalls: number
  /** false 表示"业务说他开着麦，但媒体里还没有这条轨道"（返回 null）。 */
  microphoneAvailable: boolean
  /** 让 `subscribeMicrophone` 抛错（§32 的订阅失败只影响能不能听到）。 */
  subscribeMicrophoneError: Error | null
  /** 被 attach 过的 `<video>`（断言"画面真的挂上去了"）。 */
  readonly attachedElements: HTMLVideoElement[]
  /** 媒体层的参与者事实（业务状态不在这里，见 §51）。 */
  participants: MediaRemoteParticipant[]
  /** false 表示"业务说该有画面，但媒体里还没有这条轨道"（返回 null）。 */
  screenAvailable: boolean
  /** false 表示"业务说他开着摄像头，但媒体里还没有这条轨道"（返回 null）。 */
  cameraAvailable: boolean
  /**
   * 让 subscribeScreen 抛错（订阅失败 → 卡片显示失败 + 可重试）。
   *
   * 与 connectError 一样读写整个 harness 共享的开关：房间是懒创建的，
   * 测试往往需要在房间出现之前就把失败打开。
   */
  subscribeError: Error | null
  /**
   * 让 `subscribeCamera` 抛错（§24）。
   *
   * 与 `subscribeError` **分开**：Phase 9 要证明的正是"摄像头订阅失败只影响画中画，
   * 屏幕那条照常"。共用一个开关就永远测不出这条性质。
   */
  subscribeCameraError: Error | null
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
    subscribeCameraError: Error | null
    subscribeMicrophoneError: Error | null
    /**
     * 订阅闸门：非 null 时 `subscribeScreen` 会一直挂到它 resolve。
     *
     * 用来验证"订阅请求是**并发**发出的"。真实场景里轨道要几百毫秒才推下来，
     * 如果实现是串行 await，第 N 张卡片要等前面 N-1 条轨道全部到位才开始请求，
     * 一面 20 人的监督墙就得十几秒才填满——这个 bug 在假房间里只有靠闸门才看得见。
     */
    subscribeGate: Promise<void> | null
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
    subscribeCameraError: null as Error | null,
    subscribeMicrophoneError: null as Error | null,
    subscribeGate: null as Promise<void> | null,
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
  subscribeCameraError: Error | null
  subscribeMicrophoneError: Error | null
  subscribeGate: Promise<void> | null
}): FakeMonitorRoom {
  const participantsChanged: (() => void)[] = []
  const subscribed: ((identity: string) => void)[] = []
  const unsubscribed: ((identity: string) => void)[] = []
  const disconnected: ((reason: MediaDisconnectReason) => void)[] = []

  const subscribeCalls: string[] = []
  const subscribeQualities: ScreenQuality[] = []
  const qualityChanges: { identity: string; quality: ScreenQuality }[] = []
  const unsubscribeCalls: string[] = []
  const attachedElements: HTMLVideoElement[] = []
  const subscribeCameraCalls: string[] = []
  const unsubscribeCameraCalls: string[] = []
  const cameraAttachedElements: HTMLVideoElement[] = []
  const subscribeMicrophoneCalls: string[] = []
  const unsubscribeMicrophoneCalls: string[] = []
  const microphoneAttachedElements: HTMLAudioElement[] = []
  const publishedMicrophoneTracks: MediaStreamTrack[] = []
  let unpublishMicrophoneCalls = 0
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
    subscribeScreen(
      identity: string,
      quality: ScreenQuality = 'low',
    ): Promise<ScreenSubscription | null> {
      // 调用**发起**的顺序与画质在这里就记下来：闸门开着时（订阅还没结果）
      // 也能量出"请求是不是并发发出的"。
      subscribeCalls.push(identity)
      subscribeQualities.push(quality)
      const respond = (): Promise<ScreenSubscription | null> => {
        if (flags.subscribeError) return Promise.reject(flags.subscribeError)
        // 与真实适配层一致：房间里根本没有这个参与者时返回 null
        // （业务状态可能领先于媒体状态，此时卡片显示"正在订阅画面…"）。
        if (!self.participants.some((participant) => participant.identity === identity)) {
          return Promise.resolve(null)
        }
        if (!self.screenAvailable) return Promise.resolve(null)
        const current: { quality: ScreenQuality } = { quality }
        const subscription: ScreenSubscription = {
          identity,
          attach(element: HTMLVideoElement): () => void {
            attachedElements.push(element)
            return () => {
              const index = attachedElements.indexOf(element)
              if (index >= 0) attachedElements.splice(index, 1)
            }
          },
          setQuality(next: ScreenQuality): void {
            // 与真实适配层一样对重复档位免疫：测试要断言的正是"没有多余的切换"。
            if (current.quality === next) return
            current.quality = next
            qualityChanges.push({ identity, quality: next })
          },
        }
        return Promise.resolve(subscription)
      }
      return flags.subscribeGate === null ? respond() : flags.subscribeGate.then(respond)
    },
    unsubscribeScreen(identity: string): Promise<void> {
      unsubscribeCalls.push(identity)
      return Promise.resolve()
    },
    subscribeCamera(identity: string): Promise<CameraSubscription | null> {
      // 与真实适配层一致：调用**不去重**，幂等是 store 的责任（这里正是要测它）。
      subscribeCameraCalls.push(identity)
      if (flags.subscribeCameraError) return Promise.reject(flags.subscribeCameraError)
      if (!self.participants.some((participant) => participant.identity === identity)) {
        return Promise.resolve(null)
      }
      // 摄像头可能还没发布（业务状态领先于媒体状态）：返回 null，界面不画小窗。
      if (!self.cameraAvailable) return Promise.resolve(null)
      const subscription: CameraSubscription = {
        identity,
        attach(element: HTMLVideoElement): () => void {
          cameraAttachedElements.push(element)
          return () => {
            const index = cameraAttachedElements.indexOf(element)
            if (index >= 0) cameraAttachedElements.splice(index, 1)
          }
        },
      }
      return Promise.resolve(subscription)
    },
    unsubscribeCamera(identity: string): Promise<void> {
      unsubscribeCameraCalls.push(identity)
      return Promise.resolve()
    },
    publishMicrophoneTrack(track: MediaStreamTrack): Promise<void> {
      publishedMicrophoneTracks.push(track)
      return Promise.resolve()
    },
    unpublishMicrophoneTrack(): Promise<void> {
      unpublishMicrophoneCalls += 1
      return Promise.resolve()
    },
    subscribeMicrophone(identity: string): Promise<MicrophoneSubscription | null> {
      // 与另外两条一样不去重：store 的幂等防线正是要在这里被验证。
      subscribeMicrophoneCalls.push(identity)
      if (flags.subscribeMicrophoneError) return Promise.reject(flags.subscribeMicrophoneError)
      if (!self.participants.some((participant) => participant.identity === identity)) {
        return Promise.resolve(null)
      }
      // 麦克风可能还没发布（业务状态领先于媒体状态）：返回 null，界面显示"正在连接…"。
      if (!self.microphoneAvailable) return Promise.resolve(null)
      const subscription: MicrophoneSubscription = {
        identity,
        attach(element: HTMLAudioElement): () => void {
          microphoneAttachedElements.push(element)
          return () => {
            const index = microphoneAttachedElements.indexOf(element)
            if (index >= 0) microphoneAttachedElements.splice(index, 1)
          }
        },
      }
      return Promise.resolve(subscription)
    },
    unsubscribeMicrophone(identity: string): Promise<void> {
      unsubscribeMicrophoneCalls.push(identity)
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
    subscribeQualities,
    qualityChanges,
    unsubscribeCalls,
    attachedElements,
    subscribeCameraCalls,
    unsubscribeCameraCalls,
    cameraAttachedElements,
    subscribeMicrophoneCalls,
    unsubscribeMicrophoneCalls,
    microphoneAttachedElements,
    publishedMicrophoneTracks,
    get unpublishMicrophoneCalls() {
      return unpublishMicrophoneCalls
    },
    participants: [],
    screenAvailable: true,
    cameraAvailable: true,
    microphoneAvailable: true,
    get subscribeError() {
      return flags.subscribeError
    },
    set subscribeError(next: Error | null) {
      flags.subscribeError = next
    },
    get subscribeCameraError() {
      return flags.subscribeCameraError
    },
    set subscribeCameraError(next: Error | null) {
      flags.subscribeCameraError = next
    },
    get subscribeMicrophoneError() {
      return flags.subscribeMicrophoneError
    },
    set subscribeMicrophoneError(next: Error | null) {
      flags.subscribeMicrophoneError = next
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
  resetTileVisibilityFactory()
})

/* -------------------------------------------------------------------------- */
/* 可见性替身（§52 的动态订阅）                                                */
/* -------------------------------------------------------------------------- */

export interface TileVisibilityHarness {
  /** 已经被观察过的卡片（studentId，按 observe 的先后顺序）。 */
  readonly observed: string[]
  /** 当前在视口里的卡片。 */
  readonly visible: Set<string>
  /** 把某个学生切进/切出视口（等价于老师滚动到/滚离那张卡片）。 */
  setVisible(studentId: string, visible: boolean): void
  /** 不再观察某张卡片（组件卸载）。 */
  release(studentId: string): void
}

/**
 * 装上假的可见性工厂（每个测试结束后由本文件的 afterEach 还原）。
 *
 * WHY 必须注入：happy-dom 没有布局引擎，真实 `IntersectionObserver` 永远不会回调，
 * 于是"只有可见卡片被订阅"这条本 Phase 最核心的性质在测试里根本跑不到。
 * 但真实 IO 会**在 observe 之后立刻为每个目标回调一次当前状态**，所以替身的默认
 * 行为就是"观察即视为可见"——测试若不断言可见性，看到的行为与真机上"卡片就在首屏"一致。
 */
export function installFakeVisibility(): TileVisibilityHarness {
  const listeners = new Map<string, (visible: boolean) => void>()
  const observed: string[] = []
  const visible = new Set<string>()

  setTileVisibilityFactory((studentId, onChange) => {
    observed.push(studentId)
    listeners.set(studentId, onChange)
    visible.add(studentId)
    // 真实 IO 的首次回调是异步的（下一帧），这里同步触发：调用方（卡片 → 视图 → store）
    // 本来就会把结果交给 subscribe 的异步链路，同步触发不会掩盖任何时序问题。
    onChange(true)
    const handle: TileVisibilityHandle = {
      observe(): void {
        // 替身按 studentId 记账（真实实现按元素记账）。
      },
      disconnect(): void {
        // 卡片卸载 = 它不在视口里了：两个集合都要收敛，否则"在视口里的学生"
        // 会随着老师反复进出页面慢慢变成一份历史记录。
        listeners.delete(studentId)
        visible.delete(studentId)
      },
    }
    return handle
  })

  return {
    observed,
    visible,
    setVisible(studentId: string, next: boolean): void {
      if (next) visible.add(studentId)
      else visible.delete(studentId)
      listeners.get(studentId)?.(next)
    },
    release(studentId: string): void {
      visible.delete(studentId)
      listeners.delete(studentId)
    },
  }
}

/* -------------------------------------------------------------------------- */
/* Monitor DTO 夹具（§51）                                                     */
/* -------------------------------------------------------------------------- */

/**
 * 一个"正在上课、屏幕正常"的学生。
 *
 * 默认值刻意选成**最完整**的在线形态：任何"未进入 / 已断开 / 屏幕中断"的断言都必须显式
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

/** 连接中（后端已建 Session、学生还没连上媒体，§12）的学生。 */
export function makeConnectingStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return makeMonitorStudent({
    studentId: 'student-connecting',
    displayName: '赵六',
    sessionId: 'session-connecting',
    sessionStatus: 'CONNECTING',
    screen: { active: false },
    connection: 'UNKNOWN',
    ...overrides,
  })
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

/**
 * 连过但已经断开的学生（网络掉线，可能自动恢复）。
 *
 * 注意它与"从未进入"的区别：断开的**有** sessionId，未进入的没有。
 */
export function makeDisconnectedStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return makeMonitorStudent({
    studentId: 'student-disconnected',
    displayName: '孙七',
    sessionId: 'session-disconnected',
    sessionStatus: 'DISCONNECTED',
    screen: { active: false },
    connection: 'UNKNOWN',
    ...overrides,
  })
}

/** 已经离开本次课堂的学生（LEFT）。 */
export function makeLeftStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return makeMonitorStudent({
    studentId: 'student-left',
    displayName: '周八',
    sessionId: 'session-left',
    sessionStatus: 'LEFT',
    screen: { active: false },
    ...overrides,
  })
}

/** 课堂被关闭后收尾的学生（ROOM_CLOSED）。 */
export function makeRoomClosedStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return makeMonitorStudent({
    studentId: 'student-closed',
    displayName: '吴九',
    sessionId: 'session-closed',
    sessionStatus: 'ROOM_CLOSED',
    screen: { active: false },
    ...overrides,
  })
}

/**
 * 本次 Run 从未进入课堂的学生（名单里有他，但没有 Active Session）。
 *
 * `sessionId` 与 `sessionStatus` **同时**为 null——这是冻结契约里唯一的表达方式
 * （shared-types 明确禁止为"没进过课堂"发明第 7 个状态值）。
 */
export function makeNotJoinedStudent(overrides: Partial<MonitorStudent> = {}): MonitorStudent {
  return makeMonitorStudent({
    studentId: 'student-not-joined',
    displayName: '王五',
    sessionId: null,
    sessionStatus: null,
    screen: { active: false },
    connection: 'UNKNOWN',
    joinedAt: null,
    lastEventAt: null,
    ...overrides,
  })
}
