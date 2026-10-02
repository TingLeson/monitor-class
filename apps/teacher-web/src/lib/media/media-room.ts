/**
 * 老师端媒体端口（§27 / §29 / §51 / §52）。
 *
 * 与 `apps/student-web/src/lib/media/media-room.ts` 是**刻意分开**的两份：
 * 学生端只需要"发布一条屏幕轨道"，老师端只需要"按需订阅某些学生的屏幕轨道"，
 * 两者唯一的共同点是连接选项。合成一个包会让任何一侧的扩展都要对另一侧负责，
 * 而它们的权限模型（`canPublishSources`）与失败模式完全不同。
 *
 * 这里同样是"窄接口"：没有"订阅房间里所有轨道"这样的方法。§52 要求老师端
 * `autoSubscribe = false` 并按可见性动态订阅；接口面上不给"全订阅"留入口，
 * 是让这条约束在类型层面就成立的最省事办法。
 */

/**
 * 连接质量等级（取值与 LiveKit `ConnectionQuality` 逐字相同）。
 *
 * Phase 6 不使用它渲染卡片（卡片的状态徽章来自 Monitor DTO，§51）；
 * 保留它是因为 §30 的 Focus View 需要"Network Good"这一行，而那时再改接口
 * 会牵动适配层。
 */
export type ConnectionQualityLevel = 'excellent' | 'good' | 'poor' | 'lost' | 'unknown'

/** 房间凭据（后端下发，§44）。只存内存：不进 URL / Web Storage / 日志。 */
export interface MediaCredentials {
  livekitUrl: string
  token: string
}

/** 断开原因（SDK 枚举的可读形式；数值枚举在适配层被换成名字）。 */
export type MediaDisconnectReason = string | null

/**
 * 媒体层的远端参与者。
 *
 * WHY 只有两个字段：老师端做业务判断的依据是 Monitor DTO（§51），
 * 这里只回答"房间里有没有这个人""他有没有在发布屏幕"这两个**媒体事实**。
 * 把 participant 的 metadata / name 之类暴露出去，迟早会有人拿它当业务模型用。
 */
export interface MediaRemoteParticipant {
  /** LiveKit identity = student_session UUID（§44）。 */
  identity: string
  hasScreen: boolean
}

/**
 * 一次已建立的屏幕订阅。
 *
 * `attach` 把画面挂到具体的 `<video>` 上并返回 detach：把"哪个元素"这件事留给
 * 组件（同一个订阅在 Phase 7 可能同时挂在卡片与 Focus View 上），
 * 媒体层只负责"这条轨道可以播了"。
 */
export interface ScreenSubscription {
  identity: string
  attach(element: HTMLVideoElement): () => void
}

/**
 * 老师端的媒体房间。
 *
 * `subscribeScreen()` 必须**幂等**：同一个 identity 反复调用只能产生一次订阅。
 * 这不是优化，而是正确性——Phase 6 每 10 秒刷新一次 monitor 数据，
 * 而"屏幕还在共享"这件事每次都会成立；没有幂等保护就会每 10 秒
 * `setSubscribed(true)` 一次，在服务端看来是一场订阅风暴。
 */
export interface MonitorRoom {
  /** `autoSubscribe=false` 由适配层负责（§52），调用方不需要也无法改变它。 */
  connect(): Promise<void>
  disconnect(): Promise<void>
  /** 当前远端参与者（媒体事实，不是业务状态）。 */
  participants(): MediaRemoteParticipant[]
  /** 订阅某参与者的屏幕轨道；没有屏幕发布时返回 null（调用方据此显示"等待共享"）。 */
  subscribeScreen(identity: string): Promise<ScreenSubscription | null>
  /** 取消订阅（同时停止下行，§52 的"取消不可见 Track 的订阅"）。 */
  unsubscribeScreen(identity: string): Promise<void>
  /** 有人加入/离开，或发布了新的屏幕轨道：业务状态仍以 monitor DTO 为准。 */
  onParticipantsChanged(listener: () => void): () => void
  onScreenSubscribed(listener: (identity: string) => void): () => void
  onScreenUnsubscribed(listener: (identity: string) => void): () => void
  onDisconnected(listener: (reason: MediaDisconnectReason) => void): () => void
}

export type MonitorRoomFactory = (credentials: MediaCredentials) => Promise<MonitorRoom>

/**
 * 默认工厂：动态加载真实 SDK 适配层。
 *
 * 动态 import 让测试注入替身后 `livekit-client` 完全不被加载（SDK 在 import 期
 * 就会触碰浏览器 API，happy-dom 里既慢又脆），同时生产构建里它自然成为独立 chunk。
 */
const defaultFactory: MonitorRoomFactory = async (credentials) => {
  const { createLiveKitMonitorRoom } = await import('./livekit-room.ts')
  return createLiveKitMonitorRoom(credentials)
}

/**
 * 当前工厂（依赖注入点）。
 *
 * WHY 是模块级可变值：监督墙由路由组件在 `onMounted` 里触发连接，
 * 视图没有"接收工厂"的入口。显式注入点比 `vi.mock` 整个 SDK 更不容易出事——
 * 只要测试没注入，就说明它真的在尝试连真实 SFU。
 */
let activeFactory: MonitorRoomFactory = defaultFactory

export function createMonitorRoom(credentials: MediaCredentials): Promise<MonitorRoom> {
  return activeFactory(credentials)
}

/** 注入替身工厂（仅供测试）。 */
export function setMonitorRoomFactory(factory: MonitorRoomFactory): void {
  activeFactory = factory
}

/** 还原真实工厂（测试的 afterEach 调用，避免污染下一个测试文件）。 */
export function resetMonitorRoomFactory(): void {
  activeFactory = defaultFactory
}
