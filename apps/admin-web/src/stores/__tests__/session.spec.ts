import { ApiError } from '@classwatch/api-client'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeAuthUser } from '../../__tests__/fixtures'
import { useSessionStore } from '../session'

/**
 * 会话 store 测试（§64 Frontend Test）。
 *
 * 覆盖三件最容易出错、且出错后表现为"用户莫名掉线/莫名被判定未登录"的行为：
 * 幂等 bootstrap、401 → anonymous、logout 一定清本地状态。
 */
const { fetchCurrentUserMock, loginMock, logoutMock } = vi.hoisted(() => ({
  fetchCurrentUserMock: vi.fn(),
  loginMock: vi.fn(),
  logoutMock: vi.fn(),
}))

vi.mock('../../lib/auth-api.ts', () => ({
  login: loginMock,
  fetchCurrentUser: fetchCurrentUserMock,
  logout: logoutMock,
}))

describe('管理端会话 store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    fetchCurrentUserMock.mockReset()
    loginMock.mockReset()
    logoutMock.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('初始状态是 unknown：既不能当作已登录，也不能断言未登录', () => {
    const session = useSessionStore()

    expect(session.status).toBe('unknown')
    expect(session.user).toBeNull()
    expect(session.isAuthenticated).toBe(false)
    expect(session.displayName).toBeNull()
  })

  it('bootstrap 幂等：并发调用只发一次 /auth/me', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    const session = useSessionStore()

    await Promise.all([session.bootstrap(), session.bootstrap(), session.bootstrap()])
    await session.bootstrap()

    expect(fetchCurrentUserMock).toHaveBeenCalledTimes(1)
    expect(session.status).toBe('authenticated')
    expect(session.displayName).toBe('系统管理员')
    expect(session.isAuthenticated).toBe(true)
  })

  it('bootstrap 在结果过期后会重新确认会话', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    const session = useSessionStore()
    await session.bootstrap()

    // 推进超过保鲜期（5s）：此时必须重新问后端，否则"被停用的账号"会一直显示已登录。
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 60_000)
    await session.bootstrap()

    expect(fetchCurrentUserMock).toHaveBeenCalledTimes(2)
  })

  it('401 AUTH_REQUIRED → anonymous（未登录是正常路径，不抛错）', async () => {
    fetchCurrentUserMock.mockRejectedValue(new ApiError({ code: 'AUTH_REQUIRED', status: 401 }))
    const session = useSessionStore()

    await expect(session.bootstrap()).resolves.toBeUndefined()

    expect(session.status).toBe('anonymous')
    expect(session.isAuthenticated).toBe(false)
  })

  it('403 ACCOUNT_DISABLED → anonymous（被停用的账号不得继续显示已登录）', async () => {
    fetchCurrentUserMock.mockRejectedValue(new ApiError({ code: 'ACCOUNT_DISABLED', status: 403 }))
    const session = useSessionStore()

    await session.bootstrap()

    expect(session.status).toBe('anonymous')
    expect(session.user).toBeNull()
  })

  it('403 ROLE_FORBIDDEN 同样按未登录处理（拿到别的入口的会话）', async () => {
    fetchCurrentUserMock.mockRejectedValue(new ApiError({ code: 'ROLE_FORBIDDEN', status: 403 }))
    const session = useSessionStore()

    await session.bootstrap()

    expect(session.status).toBe('anonymous')
  })

  it('网络失败 → 保持 unknown（绝不把网络抖动当成"未登录"）且下次导航会重试', async () => {
    fetchCurrentUserMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const session = useSessionStore()

    await session.bootstrap()
    expect(session.status).toBe('unknown')

    // 网络恢复：不需要等保鲜期，下一次 bootstrap 必须立刻重新请求。
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    await session.bootstrap()

    expect(fetchCurrentUserMock).toHaveBeenCalledTimes(2)
    expect(session.status).toBe('authenticated')
  })

  it('500 INTERNAL 同样保持 unknown 并允许重试（它没有回答"有没有会话"）', async () => {
    fetchCurrentUserMock.mockRejectedValue(new ApiError({ code: 'INTERNAL', status: 500 }))
    const session = useSessionStore()

    await session.bootstrap()
    expect(session.status).toBe('unknown')

    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    await session.bootstrap()
    expect(session.status).toBe('authenticated')
  })

  it('login 成功后写入用户；失败时状态不变（错误交给登录页映射文案）', async () => {
    const session = useSessionStore()

    loginMock.mockRejectedValue(new ApiError({ code: 'INVALID_CREDENTIALS', status: 401 }))
    await expect(session.login('admin', 'wrong-password')).rejects.toBeInstanceOf(ApiError)
    expect(session.status).toBe('unknown')
    expect(session.user).toBeNull()

    loginMock.mockResolvedValue(makeAuthUser('ADMIN'))
    await session.login('admin', 'pw-1234567890')

    expect(loginMock).toHaveBeenCalledWith('admin', 'pw-1234567890')
    expect(session.status).toBe('authenticated')
    expect(session.user?.account).toBe('admin')
  })

  it('logout 成功：调用后端并清空本地状态', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    logoutMock.mockResolvedValue(undefined)
    const session = useSessionStore()
    await session.bootstrap()

    const result = await session.logout()

    expect(logoutMock).toHaveBeenCalledTimes(1)
    expect(result.ok).toBe(true)
    expect(session.user).toBeNull()
    expect(session.status).toBe('anonymous')
  })

  it('logout 后端失败：仍然清空本地状态，并把失败回传给调用方', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    logoutMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const session = useSessionStore()
    await session.bootstrap()

    const result = await session.logout()

    // 本地必须退出：否则界面显示"已登录"而所有请求都在 401，用户既退不出去也做不了事。
    expect(result.ok).toBe(false)
    expect(result.error?.code).toBe('NETWORK_ERROR')
    expect(session.user).toBeNull()
    expect(session.status).toBe('anonymous')
  })

  it('logout 之后必须重新问后端，不能沿用登出前的"新鲜"结果', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    logoutMock.mockResolvedValue(undefined)
    const session = useSessionStore()
    await session.bootstrap()
    await session.logout()

    await session.bootstrap()

    expect(fetchCurrentUserMock).toHaveBeenCalledTimes(2)
  })

  it('不把用户写进任何 Web Storage（§38 禁止长期凭证进前端存储）', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    const session = useSessionStore()
    await session.bootstrap()

    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })

  it('notice 是一次性 UI 文案：可写可清，且不影响登录态', async () => {
    fetchCurrentUserMock.mockResolvedValue(makeAuthUser('ADMIN'))
    const session = useSessionStore()
    await session.bootstrap()

    session.setNotice('已退出登录')
    expect(session.notice).toBe('已退出登录')
    expect(session.isAuthenticated).toBe(true)

    session.clearNotice()
    expect(session.notice).toBeNull()
  })
})
