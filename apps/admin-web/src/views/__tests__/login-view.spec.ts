import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../../router'
import LoginView from '../LoginView.vue'

/**
 * 管理员登录页测试（§40 / §58）。
 *
 * 与"能提交"同样重要的是**它必须真的是密码登录**：密码框的 type 必须是 password、
 * autocomplete 必须是 current-password（写错会让浏览器密码管理器不填充、
 * 反而弹"保存新密码"，这是登录表单最常见也最难自查的可用性缺陷）。
 *
 * WHY 直接 mock 会话 store 而不是 mock 它内部的 auth-api：这一层验证的是视图契约
 * （把输入交给了谁、错误码翻译成了什么）；store 自己的行为由
 * stores/__tests__/session.spec.ts 覆盖。
 */
const { sessionMock } = await vi.hoisted(async () => {
  const { reactive } = await import('vue')
  return {
    sessionMock: reactive({
      status: 'anonymous' as 'unknown' | 'anonymous' | 'authenticated',
      notice: null as string | null,
      bootstrap: vi.fn(),
      login: vi.fn(),
      clearNotice: vi.fn(),
    }),
  }
})

vi.mock('../../stores/session', () => ({
  useSessionStore: () => sessionMock,
}))

async function mountLogin(query = ''): Promise<{ wrapper: VueWrapper; router: Router }> {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(`/admin/login${query}`)
  await router.isReady()
  const wrapper = mount(LoginView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

/**
 * 填表并提交，等到登录真的被调用。
 *
 * WHY 不能只 await flushPromises()：提交处理器里第一次 await 会跨过一个动态
 * import 边界，需要多一个宏任务才会 resolve；flushPromises 只清微任务队列，
 * 会早于请求发出就返回。
 */
async function submitAndWait(
  wrapper: VueWrapper,
  account: string,
  password: string,
): Promise<void> {
  const inputs = wrapper.findAll('input')
  await inputs[0]!.setValue(account)
  await inputs[1]!.setValue(password)
  await wrapper.find('form').trigger('submit')
  await vi.waitFor(() => expect(sessionMock.login).toHaveBeenCalled())
  await flushPromises()
}

function passwordInput(wrapper: VueWrapper) {
  return wrapper.find('input[type="password"]')
}

describe('管理员登录页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    sessionMock.notice = null
    sessionMock.bootstrap.mockReset().mockResolvedValue(undefined)
    sessionMock.login.mockReset()
    sessionMock.clearNotice.mockReset()
  })

  it('是密码登录：密码框 type=password 且 autocomplete=current-password', async () => {
    const { wrapper } = await mountLogin()

    expect(wrapper.findAll('input')).toHaveLength(2)
    const password = passwordInput(wrapper)
    expect(password.exists()).toBe(true)
    expect(password.attributes('autocomplete')).toBe('current-password')
    // 账号框同样需要正确的 autocomplete，否则密码管理器无法一次填充两个字段。
    expect(wrapper.findAll('input')[0]!.attributes('autocomplete')).toBe('username')
  })

  it('账号或密码为空时本地校验拦截，不调用 store', async () => {
    const { wrapper } = await mountLogin()

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(sessionMock.login).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('请输入账号')
    expect(wrapper.text()).toContain('请输入密码')
  })

  it('提交后把 trim 后的账号与密码交给 store.login，成功后进入工作台', async () => {
    sessionMock.login.mockResolvedValue(undefined)
    const { wrapper, router } = await mountLogin()

    await submitAndWait(wrapper, '  admin  ', 'pw-1234567890')

    expect(sessionMock.login).toHaveBeenCalledWith('admin', 'pw-1234567890')
    expect(router.currentRoute.value.name).toBe('admin-dashboard')
  })

  it('成功后跳到 redirect 指定的原目标（守卫塞进来的 query）', async () => {
    sessionMock.login.mockResolvedValue(undefined)
    const { wrapper, router } = await mountLogin('?redirect=/admin/users')

    await submitAndWait(wrapper, 'admin', 'pw-1234567890')

    // 测试里装配的是不带守卫的 router，因此断言的是"组件确实跳回了原目标"；
    // 守卫如何把 redirect 写进 query 由 router/__tests__/guard.spec.ts 覆盖。
    expect(router.currentRoute.value.fullPath).toBe('/admin/users')
  })

  it('redirect 指向站外时忽略它，回落到默认首页（开放重定向防护）', async () => {
    sessionMock.login.mockResolvedValue(undefined)
    const { wrapper, router } = await mountLogin('?redirect=https://evil.example')

    await submitAndWait(wrapper, 'admin', 'pw-1234567890')

    expect(router.currentRoute.value.name).toBe('admin-dashboard')
  })

  it.each([
    ['INVALID_CREDENTIALS', 401, '账号或密码不正确'],
    ['ACCOUNT_DISABLED', 403, '账号已停用，请联系管理员'],
    ['RATE_LIMITED', 429, '尝试过于频繁，请稍后再试'],
    ['CSRF_INVALID', 403, '页面已过期，请刷新页面后重试'],
    ['NETWORK_ERROR', 0, '网络异常，请检查网络后重试'],
  ] as const)('错误码 %s（HTTP %i）→ 中文文案「%s」', async (code, status, expected) => {
    sessionMock.login.mockRejectedValue(new ApiError({ code, status }))
    const { wrapper } = await mountLogin()

    await submitAndWait(wrapper, 'admin', 'pw-1234567890')

    const error = wrapper.find('[data-testid="login-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain(expected)
  })

  it('未知错误码只显示通用提示，绝不显示状态码或原始响应文本（§58）', async () => {
    sessionMock.login.mockRejectedValue(
      new ApiError({ code: 'INTERNAL', status: 500, message: '<html>500 Internal Server Error' }),
    )
    const { wrapper } = await mountLogin()

    await submitAndWait(wrapper, 'admin', 'pw-1234567890')

    const text = wrapper.text()
    expect(text).toContain('服务器内部错误')
    expect(text).not.toContain('500')
    expect(text).not.toContain('html')
    expect(text).not.toContain('Internal Server Error')
  })

  it('登录失败后清空密码框（不把密码留在 DOM 与内存里）', async () => {
    sessionMock.login.mockRejectedValue(new ApiError({ code: 'INVALID_CREDENTIALS', status: 401 }))
    const { wrapper } = await mountLogin()

    await submitAndWait(wrapper, 'admin', 'pw-1234567890')

    expect((passwordInput(wrapper).element as HTMLInputElement).value).toBe('')
  })

  it('提交中按钮禁用，避免重复提交', async () => {
    let release: () => void = () => undefined
    sessionMock.login.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          release = resolve
        }),
    )
    const { wrapper } = await mountLogin()

    await submitAndWait(wrapper, 'admin', 'pw-1234567890')

    const button = wrapper.find('button[type="submit"]')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.attributes('aria-busy')).toBe('true')

    release()
    await flushPromises()
  })

  it('展示守卫留下的提示（例如"当前账号不属于本入口"）', async () => {
    const { wrapper } = await mountLogin()
    sessionMock.notice = '当前账号不是管理员账号，已退出登录。请使用管理员账号登录。'
    await flushPromises()

    expect(wrapper.find('[data-testid="login-notice"]').text()).toContain('不是管理员账号')
  })

  it('挂载时会再确认一次会话（上次网络失败后不需要用户手动刷新）', async () => {
    await mountLogin()

    expect(sessionMock.bootstrap).toHaveBeenCalledTimes(1)
  })

  it('不渲染指向其他入口的链接（三个入口物理分离，§5）', async () => {
    const { wrapper } = await mountLogin()
    const html = wrapper.html()

    for (const other of ['/student', '/teacher', '/admin']) {
      if (other === '/admin') continue
      expect(html).not.toContain(`href="${other}`)
    }
  })
})
