import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  makeAdminUser,
  makeAuthUser,
  makeStudent,
  makeTeacher,
  makeUserListResponse,
} from '../../__tests__/fixtures'
import { routes } from '../../router'
import { useSessionStore } from '../../stores/session'
import { useUsersStore } from '../../stores/users'
import UsersView from '../UsersView.vue'

/**
 * 账号列表页测试（§64 Frontend Test / §68）。
 *
 * mock 掉的是 lib/admin-users-api（HTTP 边界），store 与视图都是真实实现——
 * 这样"筛选写进 URL → store 请求 → 渲染结果"这条链路整体被测到，而不是各测一半。
 */
const {
  listUsersMock,
  createUserMock,
  updateDisplayNameMock,
  updateUserStatusMock,
  resetTeacherPasswordMock,
} = vi.hoisted(() => ({
  listUsersMock: vi.fn(),
  createUserMock: vi.fn(),
  updateDisplayNameMock: vi.fn(),
  updateUserStatusMock: vi.fn(),
  resetTeacherPasswordMock: vi.fn(),
}))

vi.mock('../../lib/admin-users-api.ts', () => ({
  listUsers: listUsersMock,
  createUser: createUserMock,
  updateDisplayName: updateDisplayNameMock,
  updateUserStatus: updateUserStatusMock,
  resetTeacherPassword: resetTeacherPasswordMock,
  getUser: vi.fn(),
}))

/** 登录中的管理员 == 夹具里的 ADMIN，用来覆盖"停用自己"的自我保护。 */
const ADMIN = makeAdminUser({ id: 'user-admin-1' })
const TEACHER = makeTeacher({ lastLoginAt: '2026-02-01T09:30:00Z' })
const STUDENT = makeStudent()

async function mountUsersView(
  path = '/admin/users',
): Promise<{ wrapper: VueWrapper; router: Router }> {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(UsersView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

function signInAsAdmin(): void {
  useSessionStore().setUser(makeAuthUser('ADMIN', { id: ADMIN.id, account: ADMIN.account }))
}

describe('管理端用户列表页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listUsersMock.mockReset().mockResolvedValue(makeUserListResponse([ADMIN, TEACHER, STUDENT]))
    updateDisplayNameMock.mockReset().mockResolvedValue(null)
    updateUserStatusMock.mockReset().mockResolvedValue(null)
    resetTeacherPasswordMock.mockReset()
    signInAsAdmin()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('渲染每一行的账号、显示名、角色/状态徽章、创建时间与"从未登录"', async () => {
    const { wrapper } = await mountUsersView()

    const row = wrapper.find('[data-testid="user-row-user-teacher-1"]')
    expect(row.exists()).toBe(true)
    expect(row.text()).toContain('teacher001')
    expect(row.text()).toContain('李老师')
    expect(row.text()).toContain('老师')
    expect(row.text()).toContain('启用')
    expect(row.text()).toContain('2026') // 创建时间以本地化格式展示
    expect(row.text()).toContain('2026/2/1') // 上次登录（夹具是 2026-02-01T09:30:00Z）
    // 从未登录必须友好显示，而不是空白或 "Invalid Date"。
    expect(wrapper.find('[data-testid="user-row-user-student-1"]').text()).toContain('从未登录')
    // 分页摘要显示总数（§55 列表必须给出总量）。
    expect(wrapper.find('[data-testid="pagination-summary"]').text()).toContain('共 3')
  })

  it('没有任何账号时显示空状态，而不是一张空表格', async () => {
    listUsersMock.mockResolvedValue(makeUserListResponse([]))
    const { wrapper } = await mountUsersView()

    expect(wrapper.find('[data-testid="users-empty"]').text()).toContain('还没有任何账号')
    expect(wrapper.find('table').exists()).toBe(false)
  })

  it('筛选条件没有命中时显示"没有匹配的账号"并提供清除筛选', async () => {
    listUsersMock.mockResolvedValue(makeUserListResponse([], { total: 0 }))
    const { wrapper, router } = await mountUsersView('/admin/users?q=不存在的人')

    expect(wrapper.find('[data-testid="users-empty-filtered"]').exists()).toBe(true)

    await wrapper.find('[data-testid="users-empty-filtered"] button').trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.query).toEqual({})
    expect(listUsersMock).toHaveBeenCalledTimes(2)
  })

  it('加载失败时显示错误态，且绝不透出原始响应体（§58）', async () => {
    listUsersMock.mockRejectedValue(
      new ApiError({
        code: 'INTERNAL',
        status: 500,
        message: '<html><body>500 Internal Server Error</body></html>',
      }),
    )
    const { wrapper } = await mountUsersView()

    const error = wrapper.find('[data-testid="users-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain('服务器内部错误')
    expect(wrapper.text()).not.toContain('Internal Server Error')
    expect(wrapper.text()).not.toContain('html')
    // 错误态不能与空状态混淆。
    expect(wrapper.find('[data-testid="users-empty"]').exists()).toBe(false)
  })

  it('网络失败提示检查网络，而不是"服务器内部错误"', async () => {
    listUsersMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const { wrapper } = await mountUsersView()

    expect(wrapper.find('[data-testid="users-error"]').text()).toContain('网络连接失败')
  })

  it('自己那一行的"停用"按钮被禁用，并写明原因（后端也会拒绝）', async () => {
    const { wrapper } = await mountUsersView()

    const selfToggle = wrapper.find('[data-testid="toggle-status-user-admin-1"]')
    expect(selfToggle.attributes('disabled')).toBeDefined()
    expect(selfToggle.attributes('title')).toContain('不能停用当前登录的管理员账号')
    expect(wrapper.find('[data-testid="user-row-user-admin-1"]').text()).toContain(
      '不能停用当前登录账号',
    )
    // 别人的行不受影响：前端只是拦住注定 400 的那一次点击。
    expect(
      wrapper.find('[data-testid="toggle-status-user-teacher-1"]').attributes('disabled'),
    ).toBeUndefined()
  })

  it('重置密码只出现在老师行（§4 只有"重置老师密码"）', async () => {
    const { wrapper } = await mountUsersView()

    expect(wrapper.find('[data-testid="reset-password-user-teacher-1"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="reset-password-user-admin-1"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="reset-password-user-student-1"]').exists()).toBe(false)
  })

  it('从 URL 读取筛选条件与页码（刷新/分享链接后视图保持不变）', async () => {
    const { wrapper } = await mountUsersView('/admin/users?q=ali&role=TEACHER&status=ACTIVE&page=2')

    expect(listUsersMock).toHaveBeenCalledTimes(1)
    expect(listUsersMock.mock.calls[0]?.[0]).toMatchObject({
      q: 'ali',
      role: 'TEACHER',
      status: 'ACTIVE',
      page: 2,
    })
    // 搜索框回显 URL 里的关键词。
    expect(
      (wrapper.find('[data-testid="users-search"] input').element as HTMLInputElement).value,
    ).toBe('ali')
  })

  it('URL 里的非法值既不发给后端，也不写回地址栏', async () => {
    const { router } = await mountUsersView('/admin/users?role=WIZARD&status=UNKNOWN&page=abc')

    expect(listUsersMock).toHaveBeenCalledTimes(1)
    expect(listUsersMock.mock.calls[0]?.[0]).toMatchObject({
      role: undefined,
      status: undefined,
      page: 1,
    })
    // 不写回：用户的地址栏保持原样，不会被前端悄悄改成"清洗后"的样子。
    expect(router.currentRoute.value.fullPath).toBe(
      '/admin/users?role=WIZARD&status=UNKNOWN&page=abc',
    )
  })

  it('搜索防抖：停止输入 300ms 后才带着 q 请求，并同步到 URL', async () => {
    vi.useFakeTimers()
    const { wrapper, router } = await mountUsersView()
    listUsersMock.mockClear()

    await wrapper.find('[data-testid="users-search"] input').setValue('zha')
    // 还在防抖窗口内：不能每敲一个字符就打一次接口。
    expect(listUsersMock).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(listUsersMock).toHaveBeenCalledTimes(1)
    expect(listUsersMock.mock.calls[0]?.[0]).toMatchObject({ q: 'zha', page: 1 })
    expect(router.currentRoute.value.query.q).toBe('zha')
  })

  it('翻页把 page 写进 URL 并重新请求', async () => {
    listUsersMock.mockResolvedValue(makeUserListResponse([ADMIN], { total: 120, page: 1 }))
    const { wrapper, router } = await mountUsersView()

    await wrapper.find('[data-testid="pagination-next"]').trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.query.page).toBe('2')
    expect(listUsersMock).toHaveBeenCalledTimes(2)
    expect(listUsersMock.mock.calls[1]?.[0]).toMatchObject({ page: 2 })
  })

  it('停用需要二次确认，文案写明会立即断开正在进行的课堂', async () => {
    const { wrapper } = await mountUsersView()

    await wrapper.find('[data-testid="toggle-status-user-teacher-1"]').trigger('click')

    const dialog = wrapper.find('[role="dialog"]')
    expect(dialog.text()).toContain('停用账号 teacher001')
    expect(dialog.text()).toContain('停用会立即断开该账号正在进行的课堂')

    await wrapper.find('[data-testid="confirm-status-change"]').trigger('click')
    await flushPromises()

    expect(updateUserStatusMock).toHaveBeenCalledWith('user-teacher-1', { status: 'DISABLED' })
    // 写操作成功后必须以后端为准重新拉列表（不做乐观更新）。
    expect(listUsersMock).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('后端拒绝停用时把它的原因显示出来（例如"不能停用最后一个管理员"）', async () => {
    // 另一个管理员（不是自己）：界面允许停用，但后端会因为"这是最后一个 ACTIVE 管理员"拒绝。
    const otherAdmin = makeAdminUser({ id: 'user-admin-2', account: 'admin2' })
    listUsersMock.mockResolvedValue(makeUserListResponse([ADMIN, otherAdmin]))
    updateUserStatusMock.mockRejectedValue(
      new ApiError({ code: 'INVALID_REQUEST', status: 400, message: '不能停用最后一个管理员' }),
    )
    const { wrapper } = await mountUsersView()

    await wrapper.find('[data-testid="toggle-status-user-admin-2"]').trigger('click')
    await wrapper.find('[data-testid="confirm-status-change"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="dialog-error"]').text()).toContain('不能停用最后一个管理员')
    // 失败时不刷新列表：没有成功的写操作就没有新状态。
    expect(listUsersMock).toHaveBeenCalledTimes(1)
  })

  it('编辑显示名：弹窗预填当前值，保存后以后端返回的 DTO 更新那一行', async () => {
    updateDisplayNameMock.mockResolvedValue({ ...TEACHER, displayName: '李老师（数学）' })
    const { wrapper } = await mountUsersView()

    await wrapper.find('[data-testid="rename-user-teacher-1"]').trigger('click')
    const input = wrapper.find('[role="dialog"] input')
    expect((input.element as HTMLInputElement).value).toBe('李老师')

    await input.setValue('  李老师（数学）  ')
    await wrapper.find('[data-testid="confirm-rename"]').trigger('click')
    await flushPromises()

    expect(updateDisplayNameMock).toHaveBeenCalledWith('user-teacher-1', {
      displayName: '李老师（数学）',
    })
    expect(wrapper.find('[data-testid="user-row-user-teacher-1"]').text()).toContain(
      '李老师（数学）',
    )
  })

  it('重置密码：确认后调用接口，一次性密码只展示一次，关闭后从 DOM 中消失', async () => {
    resetTeacherPasswordMock.mockResolvedValue({
      user: TEACHER,
      password: 'otp-server-generated-123',
    })
    const { wrapper } = await mountUsersView()

    await wrapper.find('[data-testid="reset-password-user-teacher-1"]').trigger('click')
    expect(wrapper.find('[role="dialog"]').text()).toContain('重置老师密码')

    await wrapper.find('[data-testid="confirm-reset-password"]').trigger('click')
    await flushPromises()

    expect(resetTeacherPasswordMock).toHaveBeenCalledWith('user-teacher-1', {})
    expect(wrapper.find('[data-testid="one-time-password"]').text()).toBe(
      'otp-server-generated-123',
    )
    expect(wrapper.find('[role="dialog"]').text()).toContain('本密码不会再次显示')

    await wrapper.find('[data-testid="close-one-time-password"]').trigger('click')
    await flushPromises()

    expect(wrapper.html()).not.toContain('otp-server-generated-123')
    expect(document.body.textContent).not.toContain('otp-server-generated-123')
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })

  it('重置密码：管理员指定密码时按输入的密码提交，并做长度校验', async () => {
    resetTeacherPasswordMock.mockResolvedValue({ user: TEACHER })
    const { wrapper } = await mountUsersView()

    await wrapper.find('[data-testid="reset-password-user-teacher-1"]').trigger('click')

    // 太短：本地就拦下，不发请求。
    await wrapper.find('[role="dialog"] input[type="password"]').setValue('short')
    await wrapper.find('[data-testid="confirm-reset-password"]').trigger('click')
    await flushPromises()
    expect(resetTeacherPasswordMock).not.toHaveBeenCalled()
    expect(wrapper.find('[role="dialog"]').text()).toContain('密码至少 12 位')

    await wrapper.find('[role="dialog"] input[type="password"]').setValue('a-long-enough-password')
    await wrapper.find('[data-testid="confirm-reset-password"]').trigger('click')
    await flushPromises()

    expect(resetTeacherPasswordMock).toHaveBeenCalledWith('user-teacher-1', {
      password: 'a-long-enough-password',
    })
    // 管理员自己指定的密码不回显：弹窗只做确认。
    expect(wrapper.find('[data-testid="one-time-password"]').exists()).toBe(false)
    expect(wrapper.find('[role="dialog"]').text()).toContain('已按你输入的内容重置')
  })

  it('新建成功后的提示来自 store（一次性），可以手动关掉', async () => {
    const store = useUsersStore()
    store.justCreatedAccount = 'S10087'
    listUsersMock.mockResolvedValue(makeUserListResponse([{ ...STUDENT, account: 'S10087' }]))

    const { wrapper } = await mountUsersView()

    expect(wrapper.find('[data-testid="created-notice"]').text()).toContain('S10087')

    await wrapper.find('[data-testid="created-notice"] button').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="created-notice"]').exists()).toBe(false)
    expect(store.justCreatedAccount).toBeNull()
  })
})
