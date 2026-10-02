import { createApiClient } from '@classwatch/api-client'
import { apiBaseUrl } from './env'

/**
 * 学生端 API 客户端实例。
 *
 * Phase 0 只配置 baseUrl；下列能力已在 @classwatch/api-client 内统一实现，
 * 不要在各 app 里重复：
 * - `credentials: 'include'`：会话只走 HttpOnly Cookie（§38/§41），
 *   长期凭证禁止写入 localStorage；
 * - 统一错误体解析 → ApiError（code/status/requestId），500 的原始 HTML 不透传（§58）；
 * - X-Request-Id 透传，便于把前端报错与后端结构化日志（§59）对齐。
 *
 * 业务方法（登录、我的课堂、join、leave、events）由后续 Phase 按需追加在本文件，
 * 方法命名与 §42 的接口路径一一对应；不要写进共享包，避免共享包变成第二个后端。
 */
export const api = createApiClient({ baseUrl: apiBaseUrl })
