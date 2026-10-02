import { ApiError } from '@classwatch/api-client'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useSessionStore } from '../../stores/session'
import { makeAuthUser } from '../../__tests__/fixtures'
import { createAuthGuard, safeRedirectTarget } from '../guard'
import { authGuardConfig, routes } from '../index'

/**
 * 路由守卫测试（§64 Frontend Test：Route Guard）。
 *
 * WHY 用 createMemoryHistory 重建 router 而不是直接用导出的单例：每条用例都必须在
 * 一个干净的 pinia 实例下断言，单例会让用例之间互相污染。复用的是生产代码里的
 * routes / authGuardConfig / createAuthGuard，守卫逻辑零改动。
 */
const { fetchCurrentUserMock, logoutMock, loginMock } = vi.hoisted(() => ({
  fetchCurrentUserMock: vi.fn(),
  logoutMock: vi.fn(),
  loginMock: vi.fn(),
}))

vi.mock('../../lib/auth-api.ts', () => ({
  login: loginMock,
  fetchCurrentUser: fetchCurrentUserMock,
  logout: logoutMock,
}))

/** 用与 main.ts 完全相同的方式装配守卫。 */
function createGuardedRouter(): Router {
  const router = createRouter({ history: createMemoryHistory(), routes })
  router.beforeEach(createAuthGuard(authGuardConfig, () => useSessionStore()))
  return router
}

/** 401 AUTH_REQUIRED：没有会话（docs/auth/rbac.md §4 里这是正常路径，不是异常）。 */
function authRequired(): ApiError {
  return new ApiError({ code: 'AUTH_REQUIRED', status: 401, requestId: 'rid-test' })
}

describe('管理端路由守卫', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    fetchCurrentUserMock.mockReset()
    logoutMock.mockReset()
    loginMock.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('未登录访问受保护路由 → 重定向到本 app 登录页，并带上 redirect', async () => {
    fetchCurrentUserMock.mockRejectedValue(authRequired())
    const router = createGuardedRouter()

    await router.push('/admin/users')

    expect(router.currentRoute.value.name).toBe('admin-login')
    expect(router.currentRoute.value.query.redirect).toBe('/admin/users')
  })

  it('未登录访问带参数的受保护路由时保留完整路径（登录后能回到原处）', async () => {
    fetchCurrentUserMock.mockRejectedValue(authRequired())
    const router = createGuardedRouter()

    await router.push('/admin/users?role=TEACHER')

    expect(router.currentRoute.value.name).toBe('admin-login')
    expect(router.currentRoute.value.query.redirect).toBe('/admin/users?role=TEACHER')
  })

  it('已登录访问登录页 → 跳「我的课堂」，不停留在登录页', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    const router = createGuardedRouter()

    await router.push('/admin/login')

    expect(router.currentRoute.value.name).toBe('admin-dashboard')
  })

  it('已登录访问受保护路由 → 正常放行', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    const router = createGuardedRouter()

    await router.push('/admin/users')

    expect(router.currentRoute.value.name).toBe('admin-users')
  })

  it('角色不匹配（拿到别的入口的会话）→ 先 logout 再回登录页并留下提示（§67 跨入口拒绝）', async () => {
    // 第一次 /auth/me 返回不属于本入口的角色；登出后服务端会话已撤销，
    // 后续 /auth/me 返回 401 —— 这正是生产环境里发生的事。
    fetchCurrentUserMock
      .mockResolvedValueOnce(makeAuthUser('TEACHER'))
      .mockRejectedValue(authRequired())
    // 真实的 logout 会调用这个 mock 并随后清空本地状态（清状态那一半在
    // stores/__tests__/session.spec.ts 里覆盖）。
    logoutMock.mockResolvedValue(undefined)
    const router = createGuardedRouter()

    await router.push('/admin/users')

    expect(logoutMock).toHaveBeenCalledTimes(1)
    expect(router.currentRoute.value.name).toBe('admin-login')
    expect(useSessionStore().user).toBeNull()
    expect(useSessionStore().status).toBe('anonymous')
    expect(useSessionStore().notice).toContain('不是')
  })

  it('角色不匹配且登出失败时也不会陷入无限重定向', async () => {
    // 登出请求失败 → 服务端会话仍在 → 每次导航都还会拿到不属于本入口的用户。
    // 守卫必须只处理一次，然后把人留在登录页：否则 vue-router 会以
    // "possibly infinite redirection" 中止导航，页面直接空白。
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('TEACHER'))
    logoutMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const router = createGuardedRouter()

    await router.push('/admin/users')
    await router.push('/admin/users')

    expect(router.currentRoute.value.name).toBe('admin-login')
    expect(logoutMock).toHaveBeenCalledTimes(1)
  })

  it('会话未能确认（网络失败）时放行，不把用户挡在门外', async () => {
    fetchCurrentUserMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const router = createGuardedRouter()

    await router.push('/admin/users')

    // 授权边界在后端：一次网络抖动不该表现为"所有页面都打不开"。
    expect(router.currentRoute.value.name).toBe('admin-users')
    expect(useSessionStore().status).toBe('unknown')
  })

  it('登录页不需要登录态：未登录直接放行', async () => {
    fetchCurrentUserMock.mockRejectedValue(authRequired())
    const router = createGuardedRouter()

    await router.push('/admin/login')

    expect(router.currentRoute.value.name).toBe('admin-login')
  })

  it('多次导航只请求一次 /auth/me（守卫不会变成轮询）', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    const router = createGuardedRouter()

    await router.push('/admin/users')
    await router.push('/admin/users/new')
    await router.push('/admin/dashboard')

    expect(fetchCurrentUserMock).toHaveBeenCalledTimes(1)
  })

  it('每条路由都声明了 requiresAuth 与 title（漏写即鉴权回归）', () => {
    for (const route of routes) {
      if (route.path === '/') continue
      expect(route.meta?.requiresAuth, `${route.path} 缺少 requiresAuth`).toBeTypeOf('boolean')
      expect(route.meta?.title, `${route.path} 缺少 title`).toBeTruthy()
    }
    const loginRoute = routes.find((route) => route.path === '/admin/login')
    expect(loginRoute?.meta?.requiresAuth).toBe(false)
  })
})

describe('safeRedirectTarget（开放重定向防护）', () => {
  const LOGIN_PATH = '/admin/login'

  it.each([
    ['/admin/users', '/admin/users'],
    ['/admin/users?role=TEACHER&status=ACTIVE', '/admin/users?role=TEACHER&status=ACTIVE'],
  ])('接受站内路径 %s', (raw, expected) => {
    expect(safeRedirectTarget(raw, LOGIN_PATH)).toBe(expected)
  })

  it.each([
    ['https://evil.example', '绝对 URL'],
    ['//evil.example', '协议相对 URL'],
    ['/\\evil.example', '反斜杠绕过的协议相对 URL'],
    ['/teacher/dashboard', '别的入口的路径'],
    ['/adminx/classrooms', '前缀相似但不同入口的路径'],
    ['', '空串'],
  ])('拒绝 %s（%s）', (raw) => {
    expect(safeRedirectTarget(raw, LOGIN_PATH)).toBeNull()
  })

  it('非字符串（数组 / undefined）一并拒绝', () => {
    expect(safeRedirectTarget(undefined, LOGIN_PATH)).toBeNull()
    expect(safeRedirectTarget(['/admin/users'], LOGIN_PATH)).toBeNull()
  })

  it('恶意 redirect 被过滤后仍能正常跳到登录页', async () => {
    fetchCurrentUserMock.mockRejectedValue(authRequired())
    const router = createGuardedRouter()

    await router.push('/admin/login?redirect=https://evil.example')

    expect(router.currentRoute.value.name).toBe('admin-login')
  })
})
