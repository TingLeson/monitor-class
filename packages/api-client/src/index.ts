/**
 * @classwatch/api-client —— 三个 Web 入口共用的**传输层**客户端。
 *
 * HTTP 侧只做四件事：拼 URL、带 Cookie 会话、附加 CSRF 头、解析统一错误体抛
 * ApiError；WebSocket 侧（§47 的 Business Realtime）只做连接维持与报文解析。
 * 业务方法（登录、开课、join、哪个事件交给哪个 store）不在这里实现：它们属于
 * 各 app 的 `src/lib`，这样 shared 包不会随着后端接口增长而变成第二个后端。
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

export {
  REALTIME_AUTH_CLOSE_CODES,
  REALTIME_CONNECTION_STATES,
  RealtimeSocket,
} from './realtime-socket'
export type {
  RealtimeConnectionState,
  RealtimeLogger,
  RealtimeSocketFactory,
  RealtimeSocketLike,
  RealtimeSocketOptions,
} from './realtime-socket'
