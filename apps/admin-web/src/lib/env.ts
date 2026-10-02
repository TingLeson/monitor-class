/**
 * 类型化读取构建期环境变量。
 *
 * 只在 lib 层读 import.meta.env：其他文件各自读会散落未校验的字符串，
 * 而且一旦需要换默认值（例如接 CDN 域名）就要全局搜索。
 */
const rawApiBaseUrl = import.meta.env.VITE_API_BASE_URL

/**
 * API 根地址。
 * - 空字符串：同源。开发环境由 Vite 代理 /api 到 :8080，生产由 nginx 反代（§62）。
 * - 绝对地址：仅在前后端跨域部署时使用，此时后端必须把该来源加入 CORS allowlist，
 *   且 Cookie 需要 SameSite=None; Secure（§41 约束）。
 */
export const apiBaseUrl =
  typeof rawApiBaseUrl === 'string' ? rawApiBaseUrl.trim().replace(/\/+$/, '') : ''

/** 是否同源访问 API（决定 Cookie/CSRF 行为是否需要额外配置）。 */
export const isSameOriginApi = apiBaseUrl === ''
