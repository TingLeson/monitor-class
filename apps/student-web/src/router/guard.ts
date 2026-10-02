import type { Role } from '@classwatch/shared-types'
import type { NavigationGuardWithThis, RouteLocationNormalized, RouteLocationRaw } from 'vue-router'

/**
 * 路由守卫（Phase 1，§37 / §55 / §63 / §67）。
 *
 * ===========================================================================
 * WHY 前端守卫**不是**授权边界（§37 / §63）
 *
 * 守卫只能少发一次注定 403 的请求，属于 UX 优化。前端代码完全在用户控制之下
 * （DevTools 改 store、改打包产物、直接 curl 接口都能绕过），所以后端仍必须对
 * 每个请求独立执行 Session Middleware → Load User → ACTIVE → RBAC →
 * Resource Ownership。任何"前端挡住了就安全"的假设都会直接变成越权漏洞。
 * ===========================================================================
 */

/**
 * 守卫真正需要的最小会话接口。
 *
 * WHY 不直接依赖 store 的类型：守卫要能在测试里用普通对象注入（无需 pinia），
 * 而且显式列出这 4 个成员就等于把"守卫能用会话做什么"钉死——它只能读状态、
 * 加载会话、登出、留一条提示，不能顺手做别的事。
 */
export interface SessionLike {
  status: 'unknown' | 'anonymous' | 'authenticated'
  user: { role: Role } | null
  bootstrap: () => Promise<void>
  /** 角色不匹配时使用：先请后端撤销本入口会话，再清本地状态。 */
  logout: () => Promise<unknown>
  /** 给登录页留一条一次性提示（角色不匹配的原因）。 */
  setNotice: (message: string) => void
}

export interface AuthGuardConfig {
  /** 本 app 期望的角色。GET /auth/me 走的就是本入口的会话，因此理论上必然匹配。 */
  role: Role
  /** 登录页路由名。 */
  loginRouteName: string
  /** 登录页 path，同时用于推导"本 app 的站内路径前缀"。 */
  loginPath: string
  /** 已登录访问登录页时跳哪里。 */
  homeRouteName: string
  /** 角色不匹配时给登录页看的一次性提示。 */
  roleMismatchNotice: string
}

/**
 * 允许作为 `?redirect=` 跳转目标的路径。
 *
 * WHY 必须校验：`redirect` 来自 URL 查询串，属于外部输入。若直接
 * `router.push(redirect)`，攻击者可以构造
 * `/student/login?redirect=https://evil.example`（开放重定向：钓鱼页可以做得和
 * 登录页一模一样）或 `//evil.example`（协议相对 URL，同样会跳出去）。
 * 这里只接受"以本 app 入口前缀开头、且不是 `//` 开头"的站内路径。
 *
 * 入口前缀从登录页路径推导（`/student/login` → `/student`），这样三个 app 共用
 * 同一份实现而不必多传一个参数；三个 app 的路由表都满足"入口前缀 == 登录页
 * 路径的第一段"这一约定。
 */
export function safeRedirectTarget(raw: unknown, loginPath: string): string | null {
  if (typeof raw !== 'string' || raw === '') return null
  // 反斜杠一并拒绝：部分浏览器会把 `/\evil.com` 规范化成 `//evil.com`。
  if (raw.includes('\\')) return null
  if (!raw.startsWith('/') || raw.startsWith('//')) return null
  const entryPrefix = loginPath.slice(0, loginPath.lastIndexOf('/'))
  if (!raw.startsWith(`${entryPrefix}/`)) return null
  return raw
}

/** 构造登录页跳转目标，并带上"登录后回哪里"。 */
export function loginLocationWithRedirect(
  config: AuthGuardConfig,
  target: RouteLocationNormalized,
): RouteLocationRaw {
  const redirect = safeRedirectTarget(target.fullPath, config.loginPath)
  return { name: config.loginRouteName, query: redirect ? { redirect } : undefined }
}

/**
 * 创建认证守卫：加载会话 → 校验角色 → 重定向。
 *
 * WHY 做成工厂而不是直接写死一个 beforeEach：测试要能用 createMemoryHistory
 * 装配同一套守卫，否则"守卫是否真的拦住了未登录用户"只能靠人肉点浏览器。
 *
 * WHY 用 `() => session` 而不是直接传 store 实例：守卫是在模块加载时装配的
 * （router/index.ts 顶层），而 pinia 要到 main.ts 里 `app.use(pinia)` 之后才有
 * active 实例；直接 `useSessionStore()` 会立刻抛 "no active Pinia"，也让
 * routes.spec.ts 这类只想要路由表的测试被迫先装 pinia。
 * 守卫真正运行时（首次导航）pinia 必然已就绪——main.ts 的顺序保证了这一点。
 */
export function createAuthGuard(
  config: AuthGuardConfig,
  resolveSession: () => SessionLike,
): NavigationGuardWithThis<undefined> {
  /**
   * 是否已经为"角色不匹配"执行过一次登出。
   *
   * WHY 需要这个标志：如果登出没能真正撤销服务端会话（例如请求失败），下一次导航
   * 的 /auth/me 仍会返回同一个不属于本入口的用户，守卫就会"登出→跳登录页→再登出"
   * 无限重定向，最后被 vue-router 中止、页面一片空白。这一步只允许做一次，
   * 之后让用户在登录页正常登录即可——他本来也要换账号才能继续。
   */
  let mismatchHandled = false

  return async (to) => {
    const session = resolveSession()

    // 1) 加载会话。必须先 await：刷新页面时 pinia 是空的，直接判断
    //    isAuthenticated 会把已登录用户在每次刷新时踢回登录页。
    await session.bootstrap()

    const isLoginPage = to.name === config.loginRouteName

    // 会话未能确认（启动时网络失败 → status === 'unknown'）时一律放行：
    // 真正的授权边界在后端，页面自己会显示"连不上服务器"并允许重试。
    // WHY 不能把 unknown 当成"未登录"：那会让一次网络抖动变成"所有人被登出"，
    // 而且用户点多少次都会被弹回登录页，看起来像应用坏了。
    if (session.status === 'unknown') return true

    if (session.status === 'authenticated') {
      if (session.user && session.user.role !== config.role) {
        // 2) 角色不匹配：带着别的入口的会话访问本 app（§67 要求跨入口拒绝）。
        //    处理方式：撤销本入口会话 + 清本地状态 + 回登录页并说明原因。
        //    WHY 这不是"已登录但没权限"那种情况（后者绝不能踢回登录页，
        //    见 docs/auth/rbac.md §4）：这里前端识别到的是"这个会话不属于本入口"，
        //    用户必须换一个本入口的账号，所以唯一正确的落点就是登录页。
        //    先留提示再登出：logout() 不清 notice，但顺序反过来更容易被后人改错。
        if (!mismatchHandled) {
          mismatchHandled = true
          session.setNotice(config.roleMismatchNotice)
          await session.logout()
        }
        // 登出成功后状态已变成 anonymous，上面那条分支不会再走到这里；
        // 若登出失败（会话仍在服务端），必须**放行登录页**而不是再返回一次同样的
        // 重定向——否则 vue-router 会判定"无限重定向"并中止导航，页面直接空白。
        return isLoginPage ? true : { name: config.loginRouteName }
      }

      if (isLoginPage) {
        // 3) 已登录还去登录页 → 直接进首页，避免"登录成功后停在登录页"。
        return { name: config.homeRouteName }
      }

      return true
    }

    // status === 'anonymous'：后端明确回答"没有本入口的会话"。
    if (isLoginPage) return true

    // 4) 未登录访问受保护页面 → 登录页，并记住原目标（登录成功后跳回）。
    return loginLocationWithRedirect(config, to)
  }
}
