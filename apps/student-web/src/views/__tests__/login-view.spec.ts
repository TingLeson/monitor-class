import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../../router'
import LoginView from '../LoginView.vue'

/**
 * 学生登录页测试（§2.2 / §58）。
 *
 * 这里最重要的一条不是"能提交"，而是**页面上不允许存在密码输入框**：
 * 学生无密码是已冻结的业务规则（§2.2 逐条列出 ❌ 密码、❌ 注册、❌ 忘记密码），
 * 一旦有人"顺手"加上密码框，这条断言会立刻失败。
 *
 * WHY 直接 mock 会话 store 而不是 mock 它内部的 auth-api：这一层要验证的是视图
 * 契约——"把用户输入交给了谁、把错误码翻译成了什么"；store 自己的
 * login/logout/bootstrap 行为由 stores/__tests__/session.spec.ts 覆盖。
 * 分层清楚，也顺带避开 vitest 在"动态 import + 测试体内 mount"场景下
 * 拿到两个模块实例的坑。
 */
/**
 * 假 store。
 *
 * WHY 用 reactive 包一层：视图读的是 store 的响应式属性（notice / status），
 * 普通对象改了属性不会触发重渲染，断言会莫名其妙失败。
 * WHY 用 await import('vue') 而不是顶层 import：vi.hoisted 的回调会被提升到所有
 * import 之前执行，此时 `reactive` 还在 TDZ 里（Cannot access before initialization）。
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
  await router.push(`/student/login${query}`)
  await router.isReady()
  const wrapper = mount(LoginView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

/**
 * 提交表单并等到登录真的被调用。
 *
 * WHY 不能只 await flushPromises()：提交处理器里第一次 await 会跨过一个动态
 * import 边界，需要多一个宏任务才会 resolve；flushPromises 只清微任务队列，
 * 会早于请求发出就返回。
 */
async function submitAndWait(wrapper: VueWrapper, account: string): Promise<void> {
  await wrapper.find('input').setValue(account)
  await wrapper.find('form').trigger('submit')
  await vi.waitFor(() => expect(sessionMock.login).toHaveBeenCalled())
  await flushPromises()
}

describe('学生登录页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    sessionMock.notice = null
    sessionMock.bootstrap.mockReset().mockResolvedValue(undefined)
    sessionMock.login.mockReset()
    sessionMock.clearNotice.mockReset()
  })

  it('只有账号输入框：不存在密码框、注册链接或"忘记密码"（§2.2）', async () => {
    const { wrapper } = await mountLogin()

    expect(wrapper.findAll('input[type="password"]')).toHaveLength(0)
    expect(wrapper.findAll('input')).toHaveLength(1)

    const text = wrapper.text()
    expect(text).not.toContain('注册')
    expect(text).not.toContain('忘记密码')
    expect(text).not.toContain('验证码')
  })

  it('账号为空时本地校验拦截，不调用 store', async () => {
    const { wrapper } = await mountLogin()

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(sessionMock.login).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('请输入账号')
  })

  it('提交后把 trim 后的账号交给 store.login，成功后跳转到我的课堂', async () => {
    sessionMock.login.mockResolvedValue(undefined)
    const { wrapper, router } = await mountLogin()

    await submitAndWait(wrapper, '  S10086  ')

    expect(sessionMock.login).toHaveBeenCalledWith('S10086')
    expect(router.currentRoute.value.name).toBe('student-classrooms')
  })

  it('登录成功后回到 redirect 指定的原目标（守卫塞进来的 query）', async () => {
    sessionMock.login.mockResolvedValue(undefined)
    const { wrapper, router } = await mountLogin('?redirect=/student/classrooms/room-7')

    await submitAndWait(wrapper, 'S10086')

    // 测试里装配的是不带守卫的 router，因此断言的是"组件确实跳回了原目标"；
    // 守卫如何把 redirect 写进 query 由 router/__tests__/guard.spec.ts 覆盖。
    expect(router.currentRoute.value.fullPath).toBe('/student/classrooms/room-7')
  })

  it('redirect 指向站外时忽略它，回落到默认首页（开放重定向防护）', async () => {
    sessionMock.login.mockResolvedValue(undefined)
    const { wrapper, router } = await mountLogin('?redirect=https://evil.example')

    await submitAndWait(wrapper, 'S10086')

    expect(router.currentRoute.value.name).toBe('student-classrooms')
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

    await submitAndWait(wrapper, 'S10086')

    const error = wrapper.find('[data-testid="login-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain(expected)
  })

  it('未知错误码只显示通用提示，绝不显示状态码或原始响应文本（§58）', async () => {
    sessionMock.login.mockRejectedValue(
      new ApiError({ code: 'INTERNAL', status: 500, message: '<html>500 Internal Server Error' }),
    )
    const { wrapper } = await mountLogin()

    await submitAndWait(wrapper, 'S10086')

    const text = wrapper.text()
    expect(text).toContain('服务器内部错误')
    expect(text).not.toContain('500')
    expect(text).not.toContain('html')
    expect(text).not.toContain('Internal Server Error')
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

    await submitAndWait(wrapper, 'S10086')

    const button = wrapper.find('button[type="submit"]')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.attributes('aria-busy')).toBe('true')

    release()
    await flushPromises()
  })

  it('展示守卫留下的提示（例如"当前账号不是学生账号"）', async () => {
    const { wrapper } = await mountLogin()
    sessionMock.notice = '当前账号不是学生账号，已退出登录。请使用学生账号登录。'
    await flushPromises()

    expect(wrapper.find('[data-testid="login-notice"]').text()).toContain('不是学生账号')
  })

  it('挂载时会再确认一次会话（上次网络失败后不需要用户手动刷新）', async () => {
    await mountLogin()

    expect(sessionMock.bootstrap).toHaveBeenCalledTimes(1)
  })

  it('不渲染指向其他入口的链接（三个入口物理分离，§5）', async () => {
    const { wrapper } = await mountLogin()
    const html = wrapper.html()

    expect(html).not.toContain('href="/teacher')
    expect(html).not.toContain('href="/admin')
  })
})
