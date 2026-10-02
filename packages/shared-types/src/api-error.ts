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
  /**
   * 账号或密码错误（Phase 1 新增，HTTP 401）。
   *
   * WHY 必须与"账号不存在"共用同一个码：后端刻意不区分二者（§63 Security
   * Checklist 的反账号枚举要求），否则登录接口就成了账号枚举器。因此前端
   * **不得**据此推断账号是否存在，也不得提示"该账号未注册"。
   * 学生端没有密码，理论上收不到这个码；一旦收到说明入口或账号角色不对，
   * 仍按同一句通用文案处理，不泄漏账号状态。
   */
  'INVALID_CREDENTIALS',
  /**
   * 触发 IP / API 限流（Phase 1 新增，HTTP 429）。
   *
   * 来源是 §2.2 明确要求的 Rate Limit：学生账号无密码，限流是唯一的暴力枚举
   * 缓解手段。前端只能提示"稍后再试"，**禁止自动重试**——自动重试会让限流
   * 窗口一直无法关闭，把一次临时限流拖成持续封禁。
   */
  'RATE_LIMITED',
  /**
   * CSRF 校验失败（Phase 1 新增，HTTP 403）。
   *
   * 语义是"会话 Cookie 在，但 X-CSRF-Token 头缺失或不匹配"（§63 CSRF protection）。
   * 这**不是**未登录，因此不能当成 AUTH_REQUIRED 去清本地会话：典型成因是页面
   * 长时间停留后 CSRF Cookie 过期，刷新页面即可恢复。
   */
  'CSRF_INVALID',
  /**
   * 管理端用户管理（Phase 2 新增，§4 / §68）。
   *
   * WHY 三个码必须独立存在，而不是塌缩成一个通用 400：管理端对每个码都有具体的
   * 下一步动作——`USER_NOT_FOUND` → "这个账号已经不在了，刷新列表"；
   * `ACCOUNT_ALREADY_EXISTS` → 挂到账号输入框，让管理员换一个账号名；
   * `PASSWORD_POLICY_VIOLATION` → 挂到密码输入框并说明规则。
   * 合成一个码就等于让前端去解析后端的中文散文，契约立刻退化。
   *
   * 注意 `PASSWORD_POLICY_VIOLATION` 是 400 而不是 409：它描述的是"这份输入不满足
   * 密码策略"，与资源冲突无关——密码本身永远不会成为冲突对象。
   */
  'USER_NOT_FOUND',
  'ACCOUNT_ALREADY_EXISTS',
  'PASSWORD_POLICY_VIOLATION',
  /**
   * 管理员想停用自己当前登录的账号（409）。
   *
   * WHY 不能沿用 INVALID_REQUEST：管理界面必须把"为什么被拒绝"讲清楚——
   * 「不能停用你正在使用的账号」和「系统必须保留至少一个管理员」是完全不同的两件事，
   * 而 INVALID_REQUEST 同时覆盖了账号格式、未知字段等一堆原因。契约规定客户端按
   * `code` 分支（§58），所以需要用户理解的产品规则就该有自己的码。
   */
  'CANNOT_DISABLE_SELF',
  /** 该操作会让系统不再有任何启用状态的管理员（409）。 */
  'LAST_ADMIN_PROTECTED',
  /**
   * 请求体不合法（Phase 2 登记）。
   *
   * 后端从 Phase 1 起就在用它：登录请求体格式错误，以及"PATCH 只允许 displayName
   * 却传了 role"、"不能停用自己 / 不能停用最后一个管理员"这类被拒绝的操作都返回它。
   * 此前 shared-types 没有登记这个码，前端会把后端明确的一次 400 降级成 INTERNAL
   * （"服务器内部错误"）——明明是用户可修正的输入问题，却报成了服务故障。
   */
  'INVALID_REQUEST',
  'CLASSROOM_NOT_FOUND',
  'CLASSROOM_NOT_OWNER',
  'CLASSROOM_CLOSED',
  'CLASSROOM_ALREADY_OPEN',
  'CLASSROOM_ALREADY_CLOSED',
  'STUDENT_NOT_ASSIGNED',
  /**
   * 学生名单维护（Phase 3 新增，§11）。
   *
   * WHY 这两个码必须独立存在，而不是塌缩进 INVALID_REQUEST：批量添加是**部分成功**的，
   * 每一行 rejected 都要告诉老师"该改哪里"——`STUDENT_NOT_FOUND` → 核对账号拼写；
   * `NOT_A_STUDENT` → 拿成了老师/管理员的账号。两者的下一步动作完全不同，
   * 合成一个码就只能让前端去解析后端的中文散文（§58 契约规定客户端按 code 分支）。
   *
   * 它们也可能作为**整批拒绝**的顶层错误出现（例如只提交了一个账号）：
   * 前者 404，后者 400。
   */
  'STUDENT_NOT_FOUND',
  'NOT_A_STUDENT',
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
  // 刻意不提"密码错误"还是"账号不存在"：后端不区分，前端也不能替它区分。
  INVALID_CREDENTIALS: '账号或密码不正确。',
  // 不写具体等待秒数：后端未在契约里下发 retryAfter，编一个数字等于撒谎。
  RATE_LIMITED: '尝试过于频繁，请稍后再试。',
  CSRF_INVALID: '页面已过期，请刷新页面后重试。',

  // 账号/显示名冲突只可能是"换一个名字"，不需要管理员做别的判断。
  ACCOUNT_ALREADY_EXISTS: '该账号已存在，请换一个账号名。',
  // 账号可能刚被另一个管理员处理掉：文案要引导刷新，而不是让用户反复点同一个按钮。
  USER_NOT_FOUND: '账号不存在或已被删除，请刷新列表后重试。',
  // 具体规则由后端 message 说明（长度 / 不得与账号相同），这里只做兜底。
  PASSWORD_POLICY_VIOLATION: '密码不符合安全要求，请使用至少 12 位且不同于账号的密码。',
  CANNOT_DISABLE_SELF: '不能停用你当前登录的账号。如需停用它，请先用另一个管理员账号登录。',
  LAST_ADMIN_PROTECTED: '系统必须至少保留一个启用状态的管理员，请先创建另一个管理员。',
  // INVALID_REQUEST 的 message 由后端按场景定制（例如"不能停用自己"），
  // 前端优先展示后端那句话；这条只是它没给 message 时的兜底。
  INVALID_REQUEST: '请求未被接受，请检查填写内容后重试。',

  CLASSROOM_NOT_FOUND: '课堂不存在或已被删除。',
  CLASSROOM_NOT_OWNER: '只有课堂的创建老师可以执行该操作。',
  CLASSROOM_CLOSED: '课堂尚未开启或已经关闭。',
  CLASSROOM_ALREADY_OPEN: '课堂已经处于开启状态。',
  CLASSROOM_ALREADY_CLOSED: '课堂已经处于关闭状态。',

  STUDENT_NOT_ASSIGNED: '你不在该课堂的学生名单中。',
  // 逐条拒绝原因（批量添加）与顶层错误共用这两句：老师要做的动作是同一个。
  STUDENT_NOT_FOUND: '账号不存在，请核对账号。',
  NOT_A_STUDENT: '该账号不是学生账号。',

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

/**
 * 按错误码取"可以直接展示给用户"的中文文案（§58）。
 *
 * WHY 放在共享包而不是各 app 的 LoginView 里：三个入口的登录失败文案必须完全一致
 * （同一个 INVALID_CREDENTIALS 在教师端说"密码错误"、在管理端说"账号不存在"，
 * 就等于用两个入口的差异把账号存在性泄漏出去）。调用方只允许在"本地校验失败"
 * 这类与后端无关的场景下传自己的兜底文案。
 */
export function apiErrorMessage(code: ApiErrorCode, fallback?: string): string {
  const message = API_ERROR_MESSAGES[code]
  if (message) return message
  // 类型上不可达；保留兜底是为了运行时后端新增错误码降级后的防御性分支。
  return fallback ?? API_ERROR_MESSAGES.INTERNAL
}

export function isFrontendLocalErrorCode(value: unknown): value is FrontendLocalErrorCode {
  return (
    typeof value === 'string' && (FRONTEND_LOCAL_ERROR_CODES as readonly string[]).includes(value)
  )
}
