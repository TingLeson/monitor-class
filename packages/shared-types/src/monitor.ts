import type { IsoDateTime, Uuid } from './common'
import type { StudentSessionStatus } from './session'

/**
 * 连接质量（§51 `connection` 字段 / §52 网络策略）。
 *
 * UNKNOWN 用于"尚未收到 LiveKit 连接质量事件"的窗口期（Phase 7 首次订阅时），
 * 不要用 GOOD 兜底，否则老师会看到假的"正常"。
 */
export const CONNECTION_QUALITIES = ['GOOD', 'FAIR', 'POOR', 'UNKNOWN'] as const

export type ConnectionQuality = (typeof CONNECTION_QUALITIES)[number]

/** 轨道是否处于发布中；屏幕/摄像头/麦克风三者都复用这个形状（§51）。 */
export interface TrackActiveState {
  active: boolean
}

/**
 * 老师端 Monitor DTO（§51）。
 *
 * WHY 必须用这个 DTO 而不是 LiveKit Participant：
 * 业务状态（是否在课、屏幕是否中断、加入时间）由后端 Session + Event 决定，
 * LiveKit Track 只是媒体层事实。二者在 UI 层合成 Monitor Card（§51 / §29），
 * 否则一旦 webhook 与 track 状态不同步，老师界面就会撒谎。
 */
export interface TeacherMonitorStudent {
  studentId: Uuid
  displayName: string
  sessionStatus: StudentSessionStatus
  screen: TrackActiveState
  camera: TrackActiveState
  microphone: TrackActiveState
  connection: ConnectionQuality
  joinedAt: IsoDateTime | null
  /** 最近一次 SessionEvent 时间，用于判断状态新鲜度。 */
  lastEventAt: IsoDateTime | null
}

export function isConnectionQuality(value: unknown): value is ConnectionQuality {
  return typeof value === 'string' && (CONNECTION_QUALITIES as readonly string[]).includes(value)
}
