/**
 * 错误码契约（§58）。
 *
 * 分成三组，因为它们的来源和"能不能重试"完全不同：
 * 1. 后端业务错误码：由 Go API 返回，`{error:{code,message},requestId}`；
 * 2. 前端本地错误码：浏览器媒体 API 失败，后端根本不知道，必须前端自己判因；
 * 3. 传输层错误码：网络失败或响应体无法解析，属于 api-client 的兜底。
 *
 * 约束（§58）：任何情况下都不能把 `500 Internal Server Error` 这类原始文本
 * 直接展示给用户，展示文案一律取自 API_ERROR_MESSAGES 或后端返回的 message。
 */

/** 后端统一业务错误码（§58 上半部分）。 */
export const BACKEND_API_ERROR_CODES = [
  'AUTH_REQUIRED',
  'ACCOUNT_DISABLED',
  'ROLE_FORBIDDEN',
  'CLASSROOM_NOT_FOUND',
  'CLASSROOM_NOT_OWNER',
  'CLASSROOM_CLOSED',
  'CLASSROOM_ALREADY_OPEN',
  'CLASSROOM_ALREADY_CLOSED',
  'STUDENT_NOT_ASSIGNED',
  'SESSION_ALREADY_ACTIVE',
  'SESSION_NOT_FOUND',
  'MEDIA_TOKEN_FAILED',
] as const

/**
 * 前端本地错误码（§58 下半部分）。
 *
 * 都来自 Screen Gate / 摄像头 / 麦克风流程（§16、§24、§25）。
 * 这些错误不发请求，因此没有 requestId，也没有后端 message。
 */
export const FRONTEND_LOCAL_ERROR_CODES = [
  'SCREEN_PERMISSION_DENIED',
  'SCREEN_NOT_MONITOR',
  'SCREEN_API_UNSUPPORTED',
  'SCREEN_TRACK_ENDED',
  'CAMERA_PERMISSION_DENIED',
  'MIC_PERMISSION_DENIED',
] as const

/** 传输层兜底码：不属于后端业务码，只在 api-client 内部产生。 */
export const TRANSPORT_ERROR_CODES = ['INTERNAL', 'NETWORK_ERROR'] as const

export const API_ERROR_CODES = [
  ...BACKEND_API_ERROR_CODES,
  ...FRONTEND_LOCAL_ERROR_CODES,
  ...TRANSPORT_ERROR_CODES,
] as const

export type BackendApiErrorCode = (typeof BACKEND_API_ERROR_CODES)[number]
export type FrontendLocalErrorCode = (typeof FRONTEND_LOCAL_ERROR_CODES)[number]
export type TransportErrorCode = (typeof TRANSPORT_ERROR_CODES)[number]
export type ApiErrorCode = (typeof API_ERROR_CODES)[number]

/**
 * 默认用户可见文案。
 *
 * 只在后端没有返回 message（或响应体无法解析）时使用；文案直接面向学生/老师，
 * 因此只描述"发生了什么 + 能做什么"，不暴露状态码、堆栈或 HTML。
 */
export const API_ERROR_MESSAGES: Record<ApiErrorCode, string> = {
  AUTH_REQUIRED: '登录状态已失效，请重新登录。',
  ACCOUNT_DISABLED: '账号已被停用，请联系管理员。',
  ROLE_FORBIDDEN: '当前账号无权执行该操作。',

  CLASSROOM_NOT_FOUND: '课堂不存在或已被删除。',
  CLASSROOM_NOT_OWNER: '只有课堂的创建老师可以执行该操作。',
  CLASSROOM_CLOSED: '课堂尚未开启或已经关闭。',
  CLASSROOM_ALREADY_OPEN: '课堂已经处于开启状态。',
  CLASSROOM_ALREADY_CLOSED: '课堂已经处于关闭状态。',

  STUDENT_NOT_ASSIGNED: '你不在该课堂的学生名单中。',

  SESSION_ALREADY_ACTIVE: '你已经在这个课堂中，请勿重复进入。',
  SESSION_NOT_FOUND: '课堂会话不存在或已结束。',

  MEDIA_TOKEN_FAILED: '获取媒体凭证失败，请稍后重试。',

  SCREEN_PERMISSION_DENIED: '未获得屏幕共享权限。进入课堂必须共享整个屏幕。',
  SCREEN_NOT_MONITOR: '必须选择"整个屏幕"，共享窗口或浏览器标签页无法进入课堂。',
  SCREEN_API_UNSUPPORTED: '当前浏览器不支持屏幕共享，请使用最新版 Chrome 或 Edge。',
  SCREEN_TRACK_ENDED: '屏幕共享已停止，需要重新共享整个屏幕才能继续上课。',
  CAMERA_PERMISSION_DENIED: '未获得摄像头权限。',
  MIC_PERMISSION_DENIED: '未获得麦克风权限。',

  INTERNAL: '服务器内部错误，请稍后重试。',
  NETWORK_ERROR: '网络连接失败，请检查网络后重试。',
}

export function isApiErrorCode(value: unknown): value is ApiErrorCode {
  return typeof value === 'string' && (API_ERROR_CODES as readonly string[]).includes(value)
}

export function isFrontendLocalErrorCode(value: unknown): value is FrontendLocalErrorCode {
  return (
    typeof value === 'string' && (FRONTEND_LOCAL_ERROR_CODES as readonly string[]).includes(value)
  )
}
