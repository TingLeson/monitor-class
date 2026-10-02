import type { HttpMethod } from './client'

/**
 * CSRF（§63 CSRF protection / §41 Session）。
 *
 * 背景：会话身份放在 HttpOnly Cookie 里，浏览器对**同源**请求会自动携带它。
 * 这带来一个副作用——第三方站点伪造的表单提交同样会带上 Cookie，而且
 * HttpOnly 恰恰让前端无法用"读取 token"来证明请求确实来自本页面。
 *
 * 因此后端采用双提交（double submit）模式：
 * - 会话 Cookie `classwatch_session_<entry>`：HttpOnly，脚本读不到，也不该读；
 * - CSRF Cookie `classwatch_session_<entry>_csrf`：**非** HttpOnly，可被脚本读取，
 *   后端要求写请求把它的值原样放进 `X-CSRF-Token` 头，并校验头与 Cookie 一致。
 *
 * WHY 三个入口的 CSRF Cookie 名必须带入口后缀：开发环境三个 app 都是
 * `localhost`，Cookie 域相同、端口不隔离 Cookie。若共用一个名字，学生端与
 * 教师端的 CSRF token 会互相覆盖，表现为"另一个入口登录后退出去就 CSRF 400"。
 */
export const CSRF_COOKIE_SUFFIX = '_csrf'

/**
 * 安全方法：不改变服务端状态，因此不需要 CSRF token。
 * 用集合而不是 `method !== 'GET'` 判断，是为了让"哪些方法是安全的"这件事
 * 显式可读，也避免以后新增 HEAD/OPTIONS 时被误判成写请求。
 */
export const SAFE_METHODS: ReadonlySet<HttpMethod> = new Set<HttpMethod>(['GET'])

/** 读取 `document.cookie` 中的某个 cookie 值；不存在（或当前环境没有 document）时返回 null。 */
export function readCookie(name: string): string | null {
  // 类型上 document 总是存在，但这个包也被 vitest 的 node 环境与 SSR 之外的
  // 构建期脚本复用；少一次 "document is not defined" 崩溃比省两行代码更值。
  if (typeof document === 'undefined' || typeof document.cookie !== 'string') return null

  // 不能用 String.includes(`${name}=`)：`a_csrf` 会被 `x_a_csrf` 误命中。
  // 必须按 '; ' 切分后精确比较键名。
  const prefix = `${name}=`
  for (const part of document.cookie.split(';')) {
    const entry = part.trim()
    if (entry.startsWith(prefix)) {
      // cookie 值理论上已由浏览器解码；这里只做一次 decodeURIComponent 兜底，
      // 遇到非法百分号编码时退回原值，绝不因为解析失败就抛错。
      const rawValue = entry.slice(prefix.length)
      try {
        return decodeURIComponent(rawValue)
      } catch {
        return rawValue
      }
    }
  }
  return null
}

/**
 * 构造一个"从指定 CSRF Cookie 读取 token"的 provider，供 createApiClient 的
 * `csrfToken` 选项使用。
 *
 * WHY 每次请求都重新读 cookie，而不是创建时读一次：登录成功后后端才会下发
 * CSRF Cookie，而 api-client 实例是在模块加载时就创建好的（早于登录）。
 * 缓存第一次读到的 null 会让"登录成功后的第一个写请求"永久缺头。
 */
export function createCsrfTokenProvider(options: { cookieName: string }): () => string | null {
  const { cookieName } = options
  return () => readCookie(cookieName)
}
