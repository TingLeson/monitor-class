import type { IsoDateTime, Uuid } from './common'

/**
 * StudentSession 状态（§12，落库的 6 个值）。
 *
 * 语义边界：
 * - CONNECTING：后端已创建 Session 并签发 LiveKit token，学生尚未真正上线；
 * - ONLINE：媒体已连接，Screen Track 正常；
 * - SCREEN_LOST：屏幕共享中断（浏览器 trackended 或 LiveKit webhook，§46）；
 * - DISCONNECTED：连接断开（网络/浏览器崩溃），可能自动恢复；
 * - LEFT：学生主动离开；
 * - ROOM_CLOSED：老师关闭课堂，后端统一收尾（§49）。
 */
export const STUDENT_SESSION_STATUSES = [
  'CONNECTING',
  'ONLINE',
  'SCREEN_LOST',
  'DISCONNECTED',
  'LEFT',
  'ROOM_CLOSED',
] as const

export type StudentSessionStatus = (typeof STUDENT_SESSION_STATUSES)[number]

/**
 * 前端 UI 阶段 = 服务端状态 + 一个纯前端状态 `PRE_JOIN`。
 *
 * WHY 单独建模：学生还在 Lobby 里确认隐私提示、等待 Screen Gate 通过时，
 * 数据库里根本没有 Session（§15 Step 1/Step 2）。`PRE_JOIN` 只存在于前端 store，
 * 禁止发给后端，也禁止写进数据库（§12 明确说明）。
 */
export type StudentSessionClientPhase = StudentSessionStatus | 'PRE_JOIN'

/**
 * 学生在一次 ClassroomRun 中的实际连接（§12）。
 *
 * 一个学生可以在一节课里断开重连；重连是否复用同一 Session 由后端决定，
 * 前端不得自行假设 sessionId 稳定，必须始终以接口返回的 sessionId 为准。
 */
export interface StudentSession {
  id: Uuid
  classroomRunId: Uuid
  studentId: Uuid
  /** LiveKit identity，必须是不透明 UUID，禁止使用姓名/学号明文（§8）。 */
  livekitIdentity: string
  status: StudentSessionStatus
  connectedAt: IsoDateTime | null
  screenStartedAt: IsoDateTime | null
  screenLostAt: IsoDateTime | null
  leftAt: IsoDateTime | null
  createdAt: IsoDateTime
  updatedAt: IsoDateTime
}

/**
 * SessionEvent 类型全集（§13）。
 *
 * 这些事件是可观测性的唯一来源（老师端 Monitor、排障、审计）。
 * 注意：只记录"发生了什么"，绝不保存屏幕截图、摄像头画面或音频内容——V1 不录制（§13 / §53）。
 */
export const SESSION_EVENT_TYPES = [
  'SESSION_CREATED',
  'PARTICIPANT_CONNECTED',
  'SCREEN_PUBLISHED',
  'SCREEN_LOST',
  'SCREEN_RESTORED',
  'CAMERA_STARTED',
  'CAMERA_STOPPED',
  'MIC_STARTED',
  'MIC_STOPPED',
  'CONNECTION_LOST',
  'CONNECTION_RESTORED',
  'TEACHER_TALK_STARTED',
  'TEACHER_TALK_ENDED',
  'STUDENT_LEFT',
  'ROOM_CLOSED',
] as const

export type SessionEventType = (typeof SESSION_EVENT_TYPES)[number]

/**
 * 事件负载。V1 只约定"JSON 对象"这一层：每类事件的具体字段由后端在 Phase 8 定稿
 * （§74），现在写死字段会制造一份必然会漂移的假契约。前端只允许读取自己声明过的键。
 */
export type SessionEventPayload = Record<string, unknown>

export interface SessionEvent {
  id: Uuid
  sessionId: Uuid
  type: SessionEventType
  payload: SessionEventPayload
  createdAt: IsoDateTime
}

export function isStudentSessionStatus(value: unknown): value is StudentSessionStatus {
  return (
    typeof value === 'string' && (STUDENT_SESSION_STATUSES as readonly string[]).includes(value)
  )
}

export function isSessionEventType(value: unknown): value is SessionEventType {
  return typeof value === 'string' && (SESSION_EVENT_TYPES as readonly string[]).includes(value)
}
