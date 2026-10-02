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
 * `sessionId` 允许为 null：课堂名单里的学生可能**从未加入**过本节课，那时后端
 * 没有任何 StudentSession 可下发，这类行在监督墙上就是"未连接"卡片，也没有
 * 任何媒体可订阅。写成必填只会逼前端在运行期靠 `undefined` 猜。
 */
export interface MonitorStudent {
  studentId: Uuid
  displayName: string
  /** LiveKit identity（= student_session UUID，§44）；从未加入过本节课时为 null。 */
  sessionId: Uuid | null
  sessionStatus: StudentSessionStatus
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

export function isConnectionQuality(value: unknown): value is ConnectionQuality {
  return typeof value === 'string' && (CONNECTION_QUALITIES as readonly string[]).includes(value)
}
