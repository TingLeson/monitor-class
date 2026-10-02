import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeStudent, makeTeacher, makeUserListResponse } from '../../__tests__/fixtures'
import { routes } from '../../router'
import { useUsersStore } from '../../stores/users'
import UserNewView from '../UserNewView.vue'

/**
 * 新建账号页测试（§2.2 / §4 / §68）。
 *
 * 这一页最容易被"顺手做错"的两件事，都必须在测试里钉死：
 * 1. 选了"学生"就**不能存在**密码输入框（学生免密是业务规则，不是可选项）；
 * 2. 服务端错误必须挂到正确的字段上（账号重复 → 账号框；密码策略 → 密码框）。
 */
const { listUsersMock, createUserMock } = vi.hoisted(() => ({
  listUsersMock: vi.fn(),
  createUserMock: vi.fn(),
}))

vi.mock('../../lib/admin-users-api.ts', () => ({
  listUsers: listUsersMock,
  createUser: createUserMock,
  updateDisplayName: vi.fn(),
  updateUserStatus: vi.fn(),
  resetTeacherPassword: vi.fn(),
  getUser: vi.fn(),
}))

async function mountUserNewView(): Promise<{ wrapper: VueWrapper; router: Router }> {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/admin/users/new')
  await router.isReady()
  const wrapper = mount(UserNewView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

async function selectRole(wrapper: VueWrapper, role: 'TEACHER' | 'STUDENT'): Promise<void> {
  await wrapper.find(`[data-testid="role-option-${role}"] input`).setValue()
  await flushPromises()
}

async function fillAndSubmit(
  wrapper: VueWrapper,
  values: { account?: string; displayName?: string; password?: string } = {},
): Promise<void> {
  const { account = 'T1001', displayName = '李老师', password } = values
  await wrapper.find('[data-testid="account-field"] input').setValue(account)
  await wrapper.find('[data-testid="display-name-field"] input').setValue(displayName)
  if (password !== undefined) {
    await wrapper.find('[data-testid="password-field"] input').setValue(password)
  }
  await wrapper.find('form').trigger('submit')
  await flushPromises()
}

describe('管理端新建账号页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listUsersMock.mockReset().mockResolvedValue(makeUserListResponse([]))
    createUserMock.mockReset().mockResolvedValue(makeStudent())
  })

  it('不提供"管理员"选项，并写明原因（§4 只允许运维创建管理员）', async () => {
    const { wrapper } = await mountUserNewView()

    expect(wrapper.find('[data-testid="role-option-ADMIN"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="role-option-TEACHER"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="role-option-STUDENT"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="no-admin-notice"]').text()).toContain('make create-admin')
  })

  it('选学生时不存在密码输入框，并说明免密是业务规则（§2.2）', async () => {
    const { wrapper } = await mountUserNewView()

    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="password-field"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="student-no-password-notice"]').text()).toContain('§2.2')
  })

  it('创建学生时请求体里没有 password 字段', async () => {
    const { wrapper, router } = await mountUserNewView()
    createUserMock.mockResolvedValue(makeStudent({ account: 'S10087', displayName: '李四' }))

    await fillAndSubmit(wrapper, { account: 'S10087', displayName: '李四' })

    expect(createUserMock).toHaveBeenCalledWith({
      account: 'S10087',
      displayName: '李四',
      role: 'STUDENT',
    })
    expect(router.currentRoute.value.name).toBe('admin-users')
  })

  it('选老师时出现密码框：type=password、autocomplete=new-password', async () => {
    const { wrapper } = await mountUserNewView()

    await selectRole(wrapper, 'TEACHER')

    const password = wrapper.find('[data-testid="password-field"] input')
    expect(password.exists()).toBe(true)
    expect(password.attributes('type')).toBe('password')
    expect(password.attributes('autocomplete')).toBe('new-password')
    expect(wrapper.find('[data-testid="password-field"]').text()).toContain('至少 12 位')
  })

  it('老师密码短于 12 位时本地拦截，不发出请求', async () => {
    const { wrapper } = await mountUserNewView()
    await selectRole(wrapper, 'TEACHER')

    await fillAndSubmit(wrapper, {
      account: 'T1001',
      displayName: '李老师',
      password: 'short-pass!',
    })

    expect(createUserMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="password-field"]').text()).toContain('密码至少 12 位')
  })

  it('创建老师成功后跳回列表，并把新账号交给 store 做提示', async () => {
    const { wrapper, router } = await mountUserNewView()
    await selectRole(wrapper, 'TEACHER')
    createUserMock.mockResolvedValue(makeTeacher({ account: 'T1001' }))

    await fillAndSubmit(wrapper, {
      account: 'T1001',
      displayName: '李老师',
      password: 'a-strong-password',
    })

    expect(createUserMock).toHaveBeenCalledWith({
      account: 'T1001',
      displayName: '李老师',
      role: 'TEACHER',
      password: 'a-strong-password',
    })
    expect(router.currentRoute.value.name).toBe('admin-users')
    // 列表页据此显示"账号 X 已创建"并高亮那一行。
    expect(useUsersStore().justCreatedAccount).toBe('T1001')
  })

  it('ACCOUNT_ALREADY_EXISTS 挂到账号字段（而不是顶部横幅）', async () => {
    const { wrapper } = await mountUserNewView()
    createUserMock.mockRejectedValue(
      new ApiError({ code: 'ACCOUNT_ALREADY_EXISTS', status: 409, message: '该账号已存在' }),
    )

    await fillAndSubmit(wrapper, { account: 'S10086', displayName: '张三' })

    expect(wrapper.find('[data-testid="account-field"]').text()).toContain('该账号已存在')
    expect(wrapper.find('[data-testid="submit-error"]').exists()).toBe(false)
    // 失败后停留在表单页，用户改账号名即可重试。
    expect(createUserMock).toHaveBeenCalledTimes(1)
  })

  it('PASSWORD_POLICY_VIOLATION 挂到密码字段', async () => {
    const { wrapper } = await mountUserNewView()
    await selectRole(wrapper, 'TEACHER')
    createUserMock.mockRejectedValue(
      new ApiError({
        code: 'PASSWORD_POLICY_VIOLATION',
        status: 400,
        message: '密码长度不能超过 128 位',
      }),
    )

    // 本地只拦"太短"和"等于账号"，超长这类规则由后端判定：服务端仍是密码策略的执行点。
    await fillAndSubmit(wrapper, {
      account: 'teacher001',
      displayName: '李老师',
      password: 'a'.repeat(200),
    })

    expect(wrapper.find('[data-testid="password-field"]').text()).toContain(
      '密码长度不能超过 128 位',
    )
    expect(wrapper.find('[data-testid="submit-error"]').exists()).toBe(false)
  })

  it('密码与账号相同时本地就拦下（后端同样禁止，提前提示省一次往返）', async () => {
    const { wrapper } = await mountUserNewView()
    await selectRole(wrapper, 'TEACHER')

    await fillAndSubmit(wrapper, {
      account: 'teacher00001',
      displayName: '李老师',
      password: 'teacher00001',
    })

    expect(createUserMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="password-field"]').text()).toContain('密码不能与账号相同')
  })

  it('没有对应字段的错误走顶部提示，且绝不透出原始响应体（§58）', async () => {
    const { wrapper } = await mountUserNewView()
    createUserMock.mockRejectedValue(
      new ApiError({
        code: 'INTERNAL',
        status: 500,
        message: '<html>500 Internal Server Error</html>',
      }),
    )

    await fillAndSubmit(wrapper, { account: 'S10087', displayName: '李四' })

    const banner = wrapper.find('[data-testid="submit-error"]')
    expect(banner.text()).toContain('服务器内部错误')
    expect(wrapper.text()).not.toContain('Internal Server Error')
    expect(wrapper.text()).not.toContain('html')
  })

  it('账号格式本地校验（与数据库 users_account_format 一致）', async () => {
    const { wrapper } = await mountUserNewView()

    await fillAndSubmit(wrapper, { account: 'ab', displayName: '张三' })
    expect(createUserMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="account-field"]').text()).toContain('3–64 位')

    await fillAndSubmit(wrapper, { account: '张三 1', displayName: '张三' })
    expect(createUserMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="account-field"]').text()).toContain('只能包含字母、数字')
  })

  it('显示名为空时本地拦截', async () => {
    const { wrapper } = await mountUserNewView()

    await fillAndSubmit(wrapper, { account: 'S10087', displayName: '   ' })

    expect(createUserMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="display-name-field"]').text()).toContain('请输入显示名')
  })

  it('从老师切回学生时清空已输入的密码（隐藏字段不该把密码留在内存里）', async () => {
    const { wrapper } = await mountUserNewView()
    await selectRole(wrapper, 'TEACHER')
    await wrapper.find('[data-testid="password-field"] input').setValue('a-strong-password')

    await selectRole(wrapper, 'STUDENT')
    await selectRole(wrapper, 'TEACHER')

    expect(
      (wrapper.find('[data-testid="password-field"] input').element as HTMLInputElement).value,
    ).toBe('')
  })

  it('提交中按钮禁用，避免重复创建账号', async () => {
    const { wrapper } = await mountUserNewView()
    let release: () => void = () => undefined
    createUserMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = () => resolve(makeStudent())
        }),
    )

    await fillAndSubmit(wrapper, { account: 'S10087', displayName: '李四' })

    const submit = wrapper.find('button[type="submit"]')
    expect(submit.attributes('disabled')).toBeDefined()
    expect(submit.attributes('aria-busy')).toBe('true')

    release()
    await flushPromises()
  })
})
