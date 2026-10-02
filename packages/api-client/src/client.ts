import { API_ERROR_MESSAGES, isApiErrorCode, isApiErrorResponse } from '@classwatch/shared-types'
import { ApiError } from './api-error'

export type HttpMethod = 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'

/** 查询参数值；null / undefined 会被整条丢弃，避免拼出 `?status=null`。 */
export type QueryValue = string | number | boolean | null | undefined

export interface ApiRequestOptions {
  query?: Record<string, QueryValue>
  headers?: Record<string, string>
  signal?: AbortSignal
  /** 透传已有的 request id（例如重试同一次操作时保持链路一致，§59）。 */
  requestId?: string
}

export interface ApiRequestWithBodyOptions extends ApiRequestOptions {
  body?: unknown
}

export interface ApiClientOptions {
  /**
   * API 根地址。生产环境是三个独立域名下的同源 `/api/v1`，因此留空字符串表示
   * "同源 + Vite 代理"是合法且推荐的开发配置（见各 app 的 vite.config.ts）。
   */
  baseUrl: string
  /**
   * fetch 实现。存在的唯一理由是测试可注入 fake fetch；
   * 业务代码不应传入，默认用 globalThis.fetch。
   */
  fetchImpl?: typeof fetch
  defaultHeaders?: Record<string, string>
  /**
   * CSRF token 钩子（Phase 11，§63 CSRF protection）。
   *
   * 后端用 HttpOnly Cookie 承载会话，浏览器会自动带上 Cookie，因此"写"请求
   * （POST/PATCH/PUT/DELETE）必须额外带一个前端可读的 CSRF token。
   * Phase 0 不实现具体机制，只预留注入点，避免 Phase 11 改动调用方代码。
   */
  csrfToken?: () => string | null | undefined
  /** 生成 request id；默认使用 crypto.randomUUID()。 */
  generateRequestId?: () => string
}

export interface ApiClient {
  get<T>(path: string, options?: ApiRequestOptions): Promise<T>
  post<T>(path: string, body?: unknown, options?: ApiRequestOptions): Promise<T>
  patch<T>(path: string, body?: unknown, options?: ApiRequestOptions): Promise<T>
  del<T>(path: string, options?: ApiRequestOptions): Promise<T>
  request<T>(method: HttpMethod, path: string, options?: ApiRequestWithBodyOptions): Promise<T>
}

export const REQUEST_ID_HEADER = 'X-Request-Id'
export const CSRF_HEADER = 'X-CSRF-Token'

const JSON_CONTENT_TYPE = 'application/json'
/** 无响应体的状态码：DELETE / leave 之类的接口会返回它们。 */
const EMPTY_BODY_STATUSES = new Set([204, 205])

function normalizeBaseUrl(baseUrl: string): string {
  return baseUrl.replace(/\/+$/, '')
}

function buildUrl(baseUrl: string, path: string, query?: Record<string, QueryValue>): string {
  const normalizedPath = path.startsWith('/') ? path : `/${path}`
  const url = `${baseUrl}${normalizedPath}`
  if (!query) return url

  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === null || value === undefined) continue
    params.append(key, String(value))
  }
  const queryString = params.toString()
  if (!queryString) return url
  return `${url}${url.includes('?') ? '&' : '?'}${queryString}`
}

function defaultGenerateRequestId(): string {
  // crypto.randomUUID 在安全上下文（https / localhost）与 Node 19+ 均可用。
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `req_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`
}

/**
 * 把非 2xx 响应转成 ApiError。
 *
 * 关键约束（§58）：无法解析成统一错误体时，只能给出 INTERNAL + 通用文案。
 * 绝不能把 500 的 HTML / 纯文本塞进 message——那既不是给用户看的，也可能泄露
 * 服务器路径、框架版本等内部信息。
 */
function buildResponseError(
  rawBody: string | null,
  status: number,
  headerRequestId: string | null,
  fallbackRequestId: string,
): ApiError {
  if (rawBody) {
    try {
      const parsed: unknown = JSON.parse(rawBody)
      if (isApiErrorResponse(parsed)) {
        // 后端新增了前端还不认识的 code 时降级为 INTERNAL，但保留 message/requestId：
        // 用户至少能看到后端给出的可读说明，排障链路由 requestId 保证。
        const code = isApiErrorCode(parsed.error.code) ? parsed.error.code : 'INTERNAL'
        const message = parsed.error.message.trim() || API_ERROR_MESSAGES[code]
        return new ApiError({
          code,
          message,
          status,
          requestId: parsed.requestId || headerRequestId || fallbackRequestId,
        })
      }
    } catch {
      // 不是 JSON：走下面的通用兜底。
    }
  }

  return new ApiError({
    code: 'INTERNAL',
    message: API_ERROR_MESSAGES.INTERNAL,
    status,
    requestId: headerRequestId ?? fallbackRequestId,
  })
}

/**
 * 创建 API 客户端。
 *
 * 会话模型（§38 / §41）：后端用 HttpOnly + Secure + SameSite Cookie 保存 opaque
 * session token，前端**只**通过 Cookie 携带身份，因此这里固定
 * `credentials: 'include'`。任何长期凭证（session token、LiveKit API Secret）
 * 都禁止放进 localStorage/sessionStorage——XSS 一旦发生就等于永久账号劫持；
 * 浏览器的 HttpOnly Cookie 至少让脚本读不到凭证本身。
 */
export function createApiClient(options: ApiClientOptions): ApiClient {
  const baseUrl = normalizeBaseUrl(options.baseUrl)
  const fetchImpl: typeof fetch =
    options.fetchImpl ?? ((input, init) => globalThis.fetch(input, init))
  const generateRequestId = options.generateRequestId ?? defaultGenerateRequestId
  const defaultHeaders = options.defaultHeaders ?? {}

  async function request<T>(
    method: HttpMethod,
    path: string,
    requestOptions: ApiRequestWithBodyOptions = {},
  ): Promise<T> {
    const requestId = requestOptions.requestId ?? generateRequestId()
    const headers: Record<string, string> = {
      ...defaultHeaders,
      Accept: JSON_CONTENT_TYPE,
      [REQUEST_ID_HEADER]: requestId,
      ...requestOptions.headers,
    }

    const hasBody = requestOptions.body !== undefined
    if (hasBody && headers['Content-Type'] === undefined) {
      headers['Content-Type'] = JSON_CONTENT_TYPE
    }

    // CSRF 只对"写"请求有意义；GET 必须保持无副作用。
    if (method !== 'GET') {
      const csrfToken = options.csrfToken?.()
      if (csrfToken) {
        headers[CSRF_HEADER] = csrfToken
      }
    }

    let response: Response
    try {
      response = await fetchImpl(buildUrl(baseUrl, path, requestOptions.query), {
        method,
        headers,
        body: hasBody ? JSON.stringify(requestOptions.body) : undefined,
        credentials: 'include',
        signal: requestOptions.signal,
      })
    } catch (cause) {
      // fetch reject：离线、DNS 失败、CORS 预检被拒、AbortError 都会走到这里。
      // 统一映射为 NETWORK_ERROR，原始异常通过 cause 保留给日志使用。
      throw new ApiError({
        code: 'NETWORK_ERROR',
        message: API_ERROR_MESSAGES.NETWORK_ERROR,
        status: 0,
        requestId,
        cause,
      })
    }

    const headerRequestId = response.headers.get(REQUEST_ID_HEADER)

    if (EMPTY_BODY_STATUSES.has(response.status)) {
      if (!response.ok) {
        throw buildResponseError(null, response.status, headerRequestId, requestId)
      }
      return undefined as T
    }

    let rawBody: string | null
    try {
      rawBody = await response.text()
    } catch {
      rawBody = null
    }

    if (!response.ok) {
      throw buildResponseError(rawBody, response.status, headerRequestId, requestId)
    }

    if (rawBody === null) {
      // 2xx 但响应体读取失败（连接在传输中被切断）：属于传输问题，不是服务器业务错误。
      throw new ApiError({
        code: 'NETWORK_ERROR',
        message: API_ERROR_MESSAGES.NETWORK_ERROR,
        status: response.status,
        requestId: headerRequestId ?? requestId,
      })
    }

    if (rawBody.trim() === '') {
      return undefined as T
    }

    try {
      return JSON.parse(rawBody) as T
    } catch {
      // 2xx 却不是 JSON：说明契约被破坏（代理插入了 HTML 等）。这里同样不透传原始
      // 文本，直接按内部错误处理，让调用方走统一错误 UI。
      throw new ApiError({
        code: 'INTERNAL',
        message: API_ERROR_MESSAGES.INTERNAL,
        status: response.status,
        requestId: headerRequestId ?? requestId,
      })
    }
  }

  return {
    request,
    get: (path, requestOptions) => request('GET', path, requestOptions),
    post: (path, body, requestOptions) => request('POST', path, { ...requestOptions, body }),
    patch: (path, body, requestOptions) => request('PATCH', path, { ...requestOptions, body }),
    del: (path, requestOptions) => request('DELETE', path, requestOptions),
  }
}
