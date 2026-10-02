/**
 * @classwatch/shared-types —— 全项目唯一的前端类型契约。
 *
 * WHY 集中在一个包：三个 Web 入口（student / teacher / admin）必须对同一份后端
 * 契约达成一致；如果各 app 自己写一份 DTO，早晚会出现"学生端认为 status 有
 * WAITING、老师端认为没有"这种漂移。任务书 §6 的领域模型与 §58 的错误码是唯一真相。
 *
 * 本包是纯源码包（无构建步骤），因此必须保持零运行时依赖、零副作用：
 * 只导出类型、常量数组与类型守卫。
 */

export type { IsoDateTime, Uuid } from './common'

export { ROLES, USER_STATUSES, isRole, isUserStatus } from './user'
export type { Role, User, UserStatus } from './user'

export { SESSION_STATUSES, isAuthUser } from './auth'
export type {
  AuthMeResponse,
  AuthUser,
  LoginResponse,
  PasswordLoginRequest,
  SessionStatus,
  StudentLoginRequest,
} from './auth'

export { CLASSROOM_STATUSES, isClassroomStatus } from './classroom'
export type { Classroom, ClassroomRun, ClassroomStatus, ClassroomStudent } from './classroom'

export {
  SESSION_EVENT_TYPES,
  STUDENT_SESSION_STATUSES,
  isSessionEventType,
  isStudentSessionStatus,
} from './session'
export type {
  SessionEvent,
  SessionEventPayload,
  SessionEventType,
  StudentSession,
  StudentSessionClientPhase,
  StudentSessionStatus,
} from './session'

export { CONNECTION_QUALITIES, isConnectionQuality } from './monitor'
export type { ConnectionQuality, TeacherMonitorStudent, TrackActiveState } from './monitor'

export {
  API_ERROR_CODES,
  API_ERROR_MESSAGES,
  BACKEND_API_ERROR_CODES,
  FRONTEND_LOCAL_ERROR_CODES,
  TRANSPORT_ERROR_CODES,
  apiErrorMessage,
  isApiErrorCode,
  isFrontendLocalErrorCode,
} from './api-error'
export type {
  ApiErrorCode,
  BackendApiErrorCode,
  FrontendLocalErrorCode,
  TransportErrorCode,
} from './api-error'

export { isApiErrorResponse } from './api-dto'
export type {
  ApiErrorResponse,
  CaptureDiagnostics,
  JoinClassroomRequest,
  JoinClassroomResponse,
} from './api-dto'
