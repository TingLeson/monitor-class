/**
 * @classwatch/api-client —— 三个 Web 入口共用的 HTTP 客户端。
 *
 * 只做四件事：拼 URL、带 Cookie 会话、附加 CSRF 头、解析统一错误体抛 ApiError。
 * 业务方法（登录、开课、join 等）不在这里实现：它们属于各 app 的 `src/lib/api.ts`，
 * 这样 shared 包不会随着后端接口增长而变成第二个后端。
 */

export { CSRF_HEADER, REQUEST_ID_HEADER, createApiClient } from './client'
export type {
  ApiClient,
  ApiClientOptions,
  ApiRequestOptions,
  ApiRequestWithBodyOptions,
  HttpMethod,
  QueryValue,
} from './client'

export { CSRF_COOKIE_SUFFIX, SAFE_METHODS, createCsrfTokenProvider, readCookie } from './csrf'

export { ApiError, isApiError } from './api-error'
export type { ApiErrorInit } from './api-error'
