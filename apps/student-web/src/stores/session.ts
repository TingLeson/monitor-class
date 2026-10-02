import { ApiError, isApiError } from '@classwatch/api-client'
import type { AuthUser, SessionStatus } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'

/**
 * 学生会话状态（§37 / §38 / §41 / §67）。
 *
 * 三态而不是布尔值，因为"还没问过后端"和"后端说没有会话"必须区分开：
 * - `unknown`       —— 尚未确认（含"网络失败，问不到"）。此时既不能渲染"已登录"
 *                      界面，也不能断言用户未登录；
 * - `anonymous`     —— 后端明确回答"没有本入口的会话"（401 / 403）；
 * - `authenticated` —— 后端返回了 user。
 *
 * WHY 不把 user 存 localStorage / sessionStorage（§38 禁止长期凭证进前端存储）：
 * 1. 会话凭证只存在于 HttpOnly Cookie，localStorage 里的 user 只是一份可能过期的
 *    副本；刷新页面仍必须用 /auth/me 重新确认，否则被停用或已登出的账号还能看到
 *    已登录界面；
 * 2. 一旦允许持久化 user，后续 Phase 极易顺手把 token 也放进去，直接违背 §38。
 */

/**
 * 会话确认结果的"保鲜期"。
 *
 * WHY 需要它：路由守卫在**每次导航**都会 await bootstrap()，没有保鲜期就会变成
 * "点一下菜单打一次 /auth/me"。但也不能只记一个"查过了"的布尔值——那会让
 * "启动时网络失败"永久卡死（见 applySessionError）。
 */
const SESSION_FRESHNESS_MS = 5_000

export const useSessionStore = defineStore('session', () => {
  const user = ref<AuthUser | null>(null)
  const status = ref<SessionStatus>('unknown')
  /** 上一次成功确认会话的时间戳（epoch ms）；0 表示从未确认过。 */
  const checkedAt = ref(0)
  /**
   * 正在进行中的 /auth/me 请求。用 shallowRef 是因为 Promise 不需要深层响应式；
   * 它存在的意义只是让并发的守卫调用共用同一个请求。
   */
  const inFlight = shallowRef<Promise<void> | null>(null)
  /**
   * 给登录页看的一次性提示（例如"当前账号不属于学生端，已退出"）。
   * 只活在内存里：它是 UI 文案而不是状态，刷新后没有保留价值。
   */
  const notice = ref<string | null>(null)

  /**
   * 登录态由"后端返回的 user"唯一决定。
   * WHY 用 computed 而不是独立的布尔字段：两个字段一旦不同步（忘记改布尔值），
   * 就会出现"界面显示已登录但请求全部 401"的诡异状态。
   */
  const isAuthenticated = computed(() => user.value !== null && status.value === 'authenticated')

  /** 顶栏展示用的名字；未登录时为 null，由调用方决定显示什么。 */
  const displayName = computed(() => user.value?.displayName ?? null)

  function setUser(nextUser: AuthUser): void {
    user.value = nextUser
    status.value = 'authenticated'
    checkedAt.value = Date.now()
    // 确认「已经在线」时清掉上一条提示：它解释的是上一次为什么掉线，
    // 现在人已在线，再显示就是陈旧消息。
    // 只在**确认成功**时清：401 与网络失败都必须保留提示，否则守卫刚写下的
    // "当前账号不是学生账号"会被登录页自己的 bootstrap 顺手抹掉，
    // 用户只会看到"莫名其妙回到了登录页"。
    notice.value = null
  }

  function clear(): void {
    user.value = null
    status.value = 'anonymous'
    // 清零：下一次导航必须重新问后端，不能沿用登出前的"新鲜"结果。
    checkedAt.value = 0
  }

  function setNotice(message: string | null): void {
    notice.value = message
  }

  function clearNotice(): void {
    notice.value = null
  }

  /**
   * 把 /auth/me 的失败翻译成状态（错误码语义见 docs/auth/rbac.md §4）。
   *
   * - 401 AUTH_REQUIRED / 403 ACCOUNT_DISABLED / 403 ROLE_FORBIDDEN：本入口没有
   *   可用会话 → anonymous；
   * - NETWORK_ERROR → **保持 unknown，且不推进 checkedAt**。理由：请求根本没到
   *   服务器，此时既不知道有没有会话，也不知道账号是否被停用。若降级成 anonymous，
   *   一次网络抖动就会变成"用户被登出"；保持 unknown 让页面显示"连不上服务器"
   *   而不是撒谎说"请重新登录"，而且网络恢复后的下一次导航会自动重试，
   *   不需要用户手动刷新；
   * - 其它（500 INTERNAL / 429 / CSRF_INVALID）同样保持 unknown 并允许重试：
   *   它们都没有回答"有没有会话"这个唯一的问题。
   */
  function applySessionError(error: unknown): void {
    if (isApiError(error) && error.isSessionAbsent && !error.isNetworkError) {
      clear()
      return
    }
    user.value = null
    status.value = 'unknown'
    checkedAt.value = 0
  }

  async function fetchSession(): Promise<void> {
    // 动态 import 只为一个目的：本 store 被路由守卫 import，而 auth-api 通过
    // lib/api.ts 创建 api-client；动态加载让这条依赖不成环，避免以后有人在
    // api.ts 里 import store 时踩到 TDZ。
    const { fetchCurrentUser } = await import('../lib/auth-api.ts')
    try {
      setUser(await fetchCurrentUser())
    } catch (error) {
      applySessionError(error)
    }
  }

  /**
   * 幂等地确认会话：只有"还没查过 / 结果过期 / 上次没查成"时才真的发请求，
   * 并发调用共用同一个 Promise（守卫在重定向链上可能被连续触发）。
   */
  async function bootstrap(): Promise<void> {
    const isFresh = Date.now() - checkedAt.value < SESSION_FRESHNESS_MS
    if (isFresh && (status.value === 'authenticated' || status.value === 'anonymous')) return
    if (inFlight.value) return inFlight.value

    const task = fetchSession().finally(() => {
      inFlight.value = null
    })
    inFlight.value = task
    return task
  }

  /**
   * 登录（§38：请求体只有 account）。
   *
   * 成功后后端已下发 HttpOnly 会话 Cookie 与可读的 CSRF Cookie，前端只记住身份。
   * 失败时**不改动任何状态**：错误原样抛给登录页，由它按错误码映射中文文案
   * （映射规则见 packages/shared-types/src/api-error.ts）。
   */
  async function login(account: string): Promise<void> {
    const { login: requestLogin } = await import('../lib/auth-api.ts')
    const nextUser = await requestLogin(account)
    setUser(nextUser)
    notice.value = null
  }

  /**
   * 登出：先请后端撤销会话，再清本地状态。
   *
   * WHY 顺序不能反，也不能"后端失败就不清本地"：真实撤销发生在服务端（§41），
   * 所以必须先发请求；但网络失败时若保留本地登录态，界面会显示"已登录"而所有
   * 请求都在 401，用户既退不出去也做不了事。因此**无论后端结果如何都清本地**，
   * 并把失败信息返回给调用方用于提示（文案由 UI 层决定，不在 store 里写死）。
   *
   * 注意这里**不清 notice**：守卫会在调用 logout() 之前把"角色不匹配"的原因写进
   * notice，清掉它就等于让用户看到一次没有任何解释的掉线。notice 由登录页在展示
   * 后消费清除（见 LoginView）。
   */
  async function logout(): Promise<{ ok: boolean; error?: ApiError }> {
    let failure: ApiError | undefined
    try {
      const { logout: requestLogout } = await import('../lib/auth-api.ts')
      await requestLogout()
    } catch (error) {
      failure = isApiError(error) ? error : new ApiError({ code: 'INTERNAL', cause: error })
    } finally {
      clear()
    }
    return failure ? { ok: false, error: failure } : { ok: true }
  }

  return {
    user,
    status,
    notice,
    isAuthenticated,
    displayName,
    setUser,
    clear,
    setNotice,
    clearNotice,
    bootstrap,
    login,
    logout,
  }
})
