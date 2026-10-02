import {
  CSRF_COOKIE_SUFFIX,
  createApiClient,
  createCsrfTokenProvider,
} from '@classwatch/api-client'
import { apiBaseUrl } from './env'

/**
 * 教师端认证相关的 Cookie 名（后端契约，见 docs/auth/rbac.md §3）。
 *
 * WHY 按入口区分名字：开发环境三个前端都跑在 `localhost`，而 Cookie 的隔离维度是
 * "域 + 路径"，**端口不隔离**。若三个入口共用一个名字，老师端登录会覆盖教师端会话，
 * 表现为"另一个入口莫名其妙掉线"。
 */
export const SESSION_COOKIE_NAME = 'classwatch_session_teacher'
export const CSRF_COOKIE_NAME = `${SESSION_COOKIE_NAME}${CSRF_COOKIE_SUFFIX}`

/**
 * 教师端 API 客户端实例。
 *
 * 下列能力已在 @classwatch/api-client 内统一实现，不要在各 app 里重复：
 * - `credentials: 'include'`：会话只走 HttpOnly Cookie（§38/§41），
 *   长期凭证禁止写入 localStorage；
 * - 所有非安全方法（POST/PUT/PATCH/DELETE）自动附加 `X-CSRF-Token`（§63）：
 *   值取自后端下发的非 HttpOnly CSRF Cookie；
 * - 统一错误体解析 → ApiError（code/status/requestId），500 的原始 HTML 不透传（§58）；
 * - X-Request-Id 透传，便于把前端报错与后端结构化日志（§59）对齐。
 *
 * 业务方法（课堂 CRUD、monitor、media-token）由后续 Phase 按需追加在本文件，
 * 认证三个方法（login / me / logout）放在 ./auth-api.ts；不要写进共享包，避免共享包变成第二个后端。
 */
export const api = createApiClient({
  baseUrl: apiBaseUrl,
  // 每次写请求都重新读一次 cookie：登录成功前这里读不到任何东西，
  // 缓存一次 null 会让"登录后的第一个写请求"永久缺 CSRF 头。
  csrfToken: createCsrfTokenProvider({ cookieName: CSRF_COOKIE_NAME }),
})
