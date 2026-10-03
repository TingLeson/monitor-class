/**
 * 学生端媒体端口（§20 / §28 / §44）。
 *
 * 这一层只做一件事：把"会话页需要的媒体能力"表达成一个**我们自己的**窄接口，
 * 让 store / 视图不直接依赖 `livekit-client`。真正的 SDK 调用全部在
 * `./livekit-room.ts` 里，并且只有默认工厂会**动态**加载它。
 *
 * WHY 要这样切一刀（而不是到处 `import { Room } from 'livekit-client'`）：
 *
 * 1. **可测**。真实 LiveKit 只能在浏览器里连上真实 SFU，happy-dom 里连不了；
 *    端口层让 store 的测试可以注入一个受控替身，去断言"publish 的是 Phase 5
 *    那条轨道""没有第二次 join""订阅不会重复"这些**业务**性质。
 * 2. **不把 SDK 拖进测试环境**。SDK 在 import 期就会触碰浏览器 API（webrtc-adapter
 *    等），静态 import 会让每个跑 store 测试的文件都去执行它。默认工厂用
 *    `import()`，所以只要注入了替身，SDK 就一个字节都不会被加载。
 * 3. **接口面窄**。SDK 有上百个成员；会话页真正需要的只有"连、发布屏幕、断开、
 *    质量与断线事件"。窄接口让"学生端不会偷偷订阅别人"这件事在类型层面就成立：
 *    这里根本没有 subscribe 方法（§26）。
 *
 * 同名的老师端文件在 `apps/teacher-web/src/lib/media/`：两边刻意不共享，
 * 因为学生端只需要"发布"，老师端只需要"按需订阅"，唯一的重叠是连接选项。
 * 把它们合并成一个包会让任何一侧的扩展都要对另一侧负责（三个 app 的隔离原则）。
 */

/**
 * 连接质量等级。
 *
 * 取值与 LiveKit `ConnectionQuality` 的字符串**逐字相同**（excellent / good /
 * poor / lost / unknown），这样适配层只是透传、不必维护映射表；但类型仍然是我们
 * 自己的，测试替身不需要 import SDK 就能造出合法值。
 *
 * §56 要求把它显示成中文 + 颜色：映射在 `../connection-quality.ts`。
 */
export type ConnectionQualityLevel = 'excellent' | 'good' | 'poor' | 'lost' | 'unknown'

/**
 * 进入媒体房间的凭据。
 *
 * 两个值都来自后端（§43 join / §42 media-token），前端不拼地址（§44）、
 * 不持有任何 key/secret。**这个对象只能存在于内存**：禁止写 Web Storage、
 * 禁止放进 URL、禁止出现在日志或错误提示里。
 */
export interface MediaCredentials {
  livekitUrl: string
  token: string
}

/** 房间断线的原因（用于给学生的提示文案；SDK 的枚举值原样透传）。 */
export type MediaDisconnectReason = string | null

/**
 * 学生端的媒体房间：只发布，不订阅（见文件头第 3 点）。
 *
 * 所有 `on*` 方法都返回**取消订阅**函数，与 `screen-capture.ts` 的 `onEnded`
 * 保持同一种风格：调用方（store）只负责保存取消函数并在退出时调用。
 *
 * 摄像头（§24）是**追加**在这个端口上的两个方法，不是第二套端口：摄像头与屏幕是
 * 同一个 participant 的两条轨道，共用同一条连接、同一份凭据、同一个断开动作。
 * 拆成两个 room 会让"离开课堂时到底断开哪个连接"变成一个需要回答的问题。
 */
export interface ScreenPublisherRoom {
  /** `autoSubscribe=false` 由适配层负责（§26/§28），调用方不需要也无法改变它。 */
  connect(): Promise<void>
  /**
   * 把**已经拿到授权**的屏幕轨道发布成 `screen_share` 源（§20）。
   *
   * 参数是 `MediaStreamTrack` 而不是 `MediaStream`：会话页必须复用 Phase 5 Gate
   * 通过的那一条轨道，绝不能再调用一次 `getDisplayMedia`（§20：那会弹第二次授权框）。
   */
  publishScreenTrack(track: MediaStreamTrack): Promise<void>
  /** 撤下屏幕轨道；不 rejoin、不停止轨道（轨道由捕获层负责释放）。 */
  unpublishScreenTrack(): Promise<void>
  /**
   * 把摄像头轨道发布成 `camera` 源（§24/§75）。
   *
   * 与屏幕那条的差别只有 source 与生命周期：屏幕必须在 `connect()` 之后立刻发布
   * （§21 的不变量），摄像头则是"学生点了才发、点了关就撤"。参数同样是现成的
   * `MediaStreamTrack`——适配层不得自己调 `getUserMedia`，否则学生会看到第二次授权框。
   */
  publishCameraTrack(track: MediaStreamTrack): Promise<void>
  /**
   * 撤下摄像头轨道；不 rejoin、不停止轨道。
   *
   * WHY 不在这里 stop：真正释放设备（让摄像头指示灯灭掉）的是采集层的 `stop()`，
   * 由 store 在"关闭摄像头"的同一段逻辑里调用。适配层只负责信令面。
   */
  unpublishCameraTrack(): Promise<void>
  /** 断开连接。刻意返回 Promise，但 `beforeunload` 里调用方会不 await 地发起它。 */
  disconnect(): Promise<void>
  connectionQuality(): ConnectionQualityLevel
  onQualityChanged(listener: (quality: ConnectionQualityLevel) => void): () => void
  onDisconnected(listener: (reason: MediaDisconnectReason) => void): () => void
  /** 网络抖动时的自动重连（LiveKit 自己会重连，我们只更新界面）。 */
  onReconnecting(listener: () => void): () => void
  onReconnected(listener: () => void): () => void
}

export type ScreenPublisherRoomFactory = (
  credentials: MediaCredentials,
) => Promise<ScreenPublisherRoom>

/**
 * 默认工厂：**动态**加载真实 SDK 适配层。
 *
 * 动态 import 的两个作用：
 * - 测试注入替身后，`livekit-client` 完全不会被加载（见文件头第 2 点）；
 * - 生产构建里 SDK 自然成为独立 chunk，学生首次进入课堂时才下载它，
 *   PreJoin 页的加载不受影响。
 */
const defaultFactory: ScreenPublisherRoomFactory = async (credentials) => {
  const { createLiveKitScreenPublisherRoom } = await import('./livekit-room.ts')
  return createLiveKitScreenPublisherRoom(credentials)
}

/**
 * 当前工厂。
 *
 * WHY 是模块级可变值而不是参数一路往下传：会话 store 由视图在 `onMounted` 时
 * 触发连接，视图没有"接收工厂"的入口（路由组件不能有必填 props）。一个显式的
 * 注入点比 `vi.mock` 整个 SDK 更不容易出事——只要测试没注入，就说明它真的在
 * 尝试连真实 SFU，而不是悄悄加载了一个假 SDK 还假装通过。
 */
let activeFactory: ScreenPublisherRoomFactory = defaultFactory

/** 按凭据创建房间（store 的唯一入口）。 */
export function createScreenPublisherRoom(
  credentials: MediaCredentials,
): Promise<ScreenPublisherRoom> {
  return activeFactory(credentials)
}

/** 注入替身工厂（仅供测试；生产代码不得调用）。 */
export function setScreenPublisherRoomFactory(factory: ScreenPublisherRoomFactory): void {
  activeFactory = factory
}

/** 还原真实工厂（测试的 afterEach 调用，避免污染下一个测试文件）。 */
export function resetScreenPublisherRoomFactory(): void {
  activeFactory = defaultFactory
}
