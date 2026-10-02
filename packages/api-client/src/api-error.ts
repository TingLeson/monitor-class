import { API_ERROR_MESSAGES, type ApiErrorCode } from '@classwatch/shared-types'

export interface ApiErrorInit {
  code: ApiErrorCode
  /** 用户可见文案；省略时使用 API_ERROR_MESSAGES 中的默认文案（§58）。 */
  message?: string
  /** HTTP 状态码；0 表示请求根本没到服务器（网络失败 / CORS 预检失败）。 */
  status?: number
  /** 后端 requestId，用于和后端结构化日志（§59）对齐；前端本地错误为 null。 */
  requestId?: string | null
  cause?: unknown
}

/**
 * 所有 API 失败的统一错误类型。
 *
 * WHY 自己定义一个 class 而不是抛 Error：
 * 1. UI 需要按 code 做分支（例如 AUTH_REQUIRED 跳登录、STUDENT_NOT_ASSIGNED 提示
 *    未授权），字符串匹配太脆弱；
 * 2. requestId 必须一路带到 UI/日志里，否则线上问题无法与后端日志关联；
 * 3. 后端可能返回 500 HTML（网关/代理层），此时 message 绝不能是原始响应文本（§58），
 *    必须由本类型统一兜底成可读文案。
 */
export class ApiError extends Error {
  readonly code: ApiErrorCode
  readonly status: number
  readonly requestId: string | null

  constructor(init: ApiErrorInit) {
    super(
      init.message ?? API_ERROR_MESSAGES[init.code],
      init.cause === undefined ? undefined : { cause: init.cause },
    )
    this.name = 'ApiError'
    this.code = init.code
    this.status = init.status ?? 0
    this.requestId = init.requestId ?? null
  }

  /** 请求没到达服务器：UI 应提示检查网络，而不是提示"服务器错误"。 */
  get isNetworkError(): boolean {
    return this.code === 'NETWORK_ERROR'
  }

  /**
   * 会话/角色问题。Phase 1 的路由守卫据此重定向到登录页；
   * 注意重定向只是 UX，真正的授权判定在后端（§37）。
   */
  get isAuthError(): boolean {
    return (
      this.code === 'AUTH_REQUIRED' ||
      this.code === 'ACCOUNT_DISABLED' ||
      this.code === 'ROLE_FORBIDDEN'
    )
  }

  /**
   * "这个入口当前没有可用会话"，也就是可以按未登录处理。
   *
   * WHY 单独一个 getter，而不是让三个 app 各自写 code 判断：
   * 1. 白名单必须一致。401 AUTH_REQUIRED（无会话/过期/已撤销）、
   *    403 ACCOUNT_DISABLED（账号被停用）、403 ROLE_FORBIDDEN（拿着别的入口的
   *    会话访问本入口，§67 要求跨入口拒绝）三种情况对"取当前用户"这个语义而言
   *    都等于"没有本入口的登录态"；
   * 2. **NETWORK_ERROR 绝不能算进来**：请求根本没到服务器，把它当成未登录会让
   *    一次网络抖动变成"用户被登出"；
   * 3. CSRF_INVALID 也绝不能算进来：会话还在，只是 token 过期，刷新即可恢复。
   *
   * 命名强调 "session"（会话）而不是 "auth"：业务接口上的 403 ROLE_FORBIDDEN
   * 表示"账号正常但无权做这件事"，那种情况**不允许**把用户踢回登录页
   * （见 docs/auth/rbac.md §4）。
   */
  get isSessionAbsent(): boolean {
    return this.isAuthError
  }
}

export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError
}
