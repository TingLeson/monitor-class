import type { IsoDateTime, Uuid } from './common'
import type { StudentSessionStatus } from './session'

/**
 * 连接质量（§51 `connection` 字段 / §52 网络策略）。
 *
 * UNKNOWN 用于"尚未收到 LiveKit 连接质量事件"的窗口期（Phase 7 首次订阅时），
 * 不要用 GOOD 兜底，否则老师会看到假的"正常"。
 *
 * Phase 6 的 Monitor DTO 实际只会下发 `GOOD` / `UNKNOWN`（后端还没有把 LiveKit 的
 * 连接质量事件落成业务状态）。这里仍然保留 FAIR / POOR：§30 的 Focus View 要显示
 * "Network Good" 这类更细的质量，而契约收窄容易、放宽难——一旦后端开始下发
 * FAIR/POOR，前端不必再改一次类型。
 */
export const CONNECTION_QUALITIES = ['GOOD', 'FAIR', 'POOR', 'UNKNOWN'] as const

export type ConnectionQuality = (typeof CONNECTION_QUALITIES)[number]

/** 轨道是否处于发布中；屏幕/摄像头/麦克风三者都复用这个形状（§51）。 */
export interface TrackActiveState {
  active: boolean
}

/**
 * 老师端 Monitor DTO（§51，Phase 6 冻结契约）。
 *
 * WHY 必须用这个 DTO 而不是 LiveKit Participant：
 * 业务状态（是否在课、屏幕是否中断、加入时间）由后端 Session + Event 决定，
 * LiveKit Track 只是媒体层事实。二者在 UI 层合成 Monitor Card（§51 / §29），
 * 否则一旦 webhook 与 track 状态不同步，老师界面就会撒谎。
 *
 * `sessionId` 是**唯一**把两边接起来的键：服务端用它当 LiveKit identity（§44 规定
 * identity = student_session UUID），老师端因此能拿它去 `remoteParticipants` 里找到
 * 对应的 participant。除此之外前端**不得**用 LiveKit 的身份做业务判断——
 * "participant 还在房间里"不等于"这个学生在上课"。
 *
 * **Phase 7 起这个 DTO 覆盖课堂名单上的全部被授权学生**，不再只有"已经进入课堂的"：
 * 于是必须能表达"这个学生本次 Run 还没进来"。表达方式只有一种——
 * `sessionId` 与 `sessionStatus` 同时为 null：
 *
 * - 没有 StudentSession 就**没有状态可下发**，所以 `sessionStatus` 允许为 null。
 *   这里刻意**不新增第 7 个枚举值**：`STUDENT_SESSION_STATUSES` 是数据库
 *   `student_sessions.status` 的落库值（§12），"没进过课堂"是"没有这一行"，
 *   不是"这一行的值是 X"。造一个 `NOT_JOINED` 状态会让"库里那条记录到底存了什么"
 *   变得没有答案，也会让前端的 `isStudentSessionStatus` 守卫开始接受一个后端永不下发的值。
 * - 两者必须**同时**为 null（不变量）。只有其中一个为 null 的行是契约被破坏，
 *   界面按"未进入"处理即可，绝不能拿它去订阅媒体——没有 identity 就没有可订的轨道。
 */
export interface MonitorStudent {
  studentId: Uuid
  displayName: string
  /** LiveKit identity（= student_session UUID，§44）；本次 Run 未进入时为 null。 */
  sessionId: Uuid | null
  /**
   * 本次 Run 的会话状态；`sessionId === null` 时为 null（未进入课堂，没有第 7 个状态）。
   */
  sessionStatus: StudentSessionStatus | null
  screen: TrackActiveState
  camera: TrackActiveState
  microphone: TrackActiveState
  connection: ConnectionQuality
  joinedAt: IsoDateTime | null
  /** 最近一次 SessionEvent 时间，用于判断状态新鲜度。 */
  lastEventAt: IsoDateTime | null
}

/**
 * Phase 0 的旧名字，保留为别名以免引用方被迫做与业务无关的改名；
 * **新代码一律用 `MonitorStudent`**，与 §51 的 DTO 名称逐字一致。
 *
 * @deprecated 使用 {@link MonitorStudent}。
 */
export type TeacherMonitorStudent = MonitorStudent

/** `GET /teacher/classrooms/:id/monitor` 的信封（§42 Teacher / §51）。 */
export interface MonitorResponse {
  students: MonitorStudent[]
}

/**
 * 监督卡片的**派生状态**（§29 / §30）——六个值，与 §12 的持久化状态一一对应，
 * 另外把"没有会话"这一种情况也收进来。
 *
 * WHY 需要一个独立于 DTO 的类型：
 * - DTO 里的 `sessionStatus` 为空、`screen.active` 为假这些**字段组合**才是老师看到的
 *   "状态"，而字段组合有 2 × N 种，界面不该在模板里散落判断（§51 的合成只做一次）；
 * - 派生状态是**纯函数**的结果，因此可以在单元测试里逐条钉死——把 `ONLINE` 但屏幕没在
 *   发布的组合显示成"正常"，是监督系统里最严重的谎报；
 * - 它是展示层词表，**不是**契约：后端永远不下发 `MonitorTileState`。
 *
 * 映射（唯一的实现见 apps/teacher-web/src/lib/monitor-status.ts）：
 *
 * | sessionStatus | screen.active | MonitorTileState |
 * | --- | --- | --- |
 * | null（未进入） | — | NOT_JOINED |
 * | CONNECTING | — | CONNECTING |
 * | ONLINE | true | NORMAL |
 * | ONLINE | false | SCREEN_LOST |
 * | SCREEN_LOST | — | SCREEN_LOST |
 * | DISCONNECTED | — | DISCONNECTED |
 * | LEFT / ROOM_CLOSED | — | LEFT |
 *
 * 注意 `LEFT` 把 `ROOM_CLOSED` 也收进来：对老师来说"学生自己走了"与"课堂被关掉了"
 * 是同一件事——这个人已经不在这节课里了（区分二者没有可采取的动作差异）。
 */
export const MONITOR_TILE_STATES = [
  'NOT_JOINED',
  'CONNECTING',
  'NORMAL',
  'SCREEN_LOST',
  'DISCONNECTED',
  'LEFT',
] as const

export type MonitorTileState = (typeof MONITOR_TILE_STATES)[number]

export function isConnectionQuality(value: unknown): value is ConnectionQuality {
  return typeof value === 'string' && (CONNECTION_QUALITIES as readonly string[]).includes(value)
}
