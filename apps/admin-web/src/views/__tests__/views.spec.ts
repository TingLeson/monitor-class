import { ApiError } from '@classwatch/api-client'
import type { AdminUserListQuery } from '@classwatch/shared-types'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import type { Component } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeUserListResponse } from '../../__tests__/fixtures'
import { routes } from '../../router'
import DashboardView from '../DashboardView.vue'
import LoginView from '../LoginView.vue'
import UserNewView from '../UserNewView.vue'
import UsersView from '../UsersView.vue'

/**
 * 管理端页面烟雾测试。
 *
 * 每个真实页面都在"真实 URL"下挂载一次：路由表、组件根节点、以及"页面确实能渲染出
 * 自己的标题"这三件事一旦被后续 Phase 改坏，这里会立刻变红。页面各自的行为细节由
 * users-view.spec.ts / user-new-view.spec.ts / login-view.spec.ts 覆盖。
 */
vi.mock('../../lib/auth-api.ts', () => ({
  login: vi.fn(),
  fetchCurrentUser: vi.fn().mockRejectedValue(new ApiError({ code: 'AUTH_REQUIRED', status: 401 })),
  logout: vi.fn().mockResolvedValue(undefined),
}))

const { listUsersMock } = vi.hoisted(() => ({ listUsersMock: vi.fn() }))

vi.mock('../../lib/admin-users-api.ts', () => ({
  listUsers: listUsersMock,
  createUser: vi.fn(),
  updateDisplayName: vi.fn(),
  updateUserStatus: vi.fn(),
  resetTeacherPassword: vi.fn(),
  getUser: vi.fn(),
}))

/** 概览页的四个统计各自按 role / status 请求，用查询条件决定返回的 total。 */
function mockCounts(totals: Partial<Record<string, number>>): void {
  listUsersMock.mockImplementation((query: AdminUserListQuery) => {
    const key = query.role ?? query.status ?? 'UNKNOWN'
    return Promise.resolve(makeUserListResponse([], { total: totals[key] ?? 0 }))
  })
}

const PAGE_CASES: { path: string; heading: string; component: Component }[] = [
  { path: '/admin/dashboard', heading: '管理概览', component: DashboardView },
  { path: '/admin/users', heading: '用户管理', component: UsersView },
  { path: '/admin/users/new', heading: '新建账号', component: UserNewView },
]

async function mountAt(path: string, component: Component) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(component, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('管理端页面骨架', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listUsersMock.mockReset().mockResolvedValue(makeUserListResponse([]))
  })

  it.each(PAGE_CASES)('$path 渲染页面标题', async ({ path, heading, component }) => {
    const { wrapper } = await mountAt(path, component)

    expect(wrapper.find('h1').text()).toContain(heading)
  })

  it.each(PAGE_CASES)('$path 不再是 Phase 占位页', async ({ path, component }) => {
    const { wrapper } = await mountAt(path, component)

    expect(wrapper.text()).not.toContain('将在 Phase')
  })

  it('登录页渲染账号与密码输入框，且不链接到其他入口（§5）', async () => {
    const { wrapper } = await mountAt('/admin/login', LoginView)
    const html = wrapper.html()

    expect(wrapper.findAll('input[type="password"]')).toHaveLength(1)
    for (const other of ['/student', '/teacher', '/admin']) {
      if (other === '/admin') continue
      expect(html).not.toContain(`href="${other}`)
    }
  })
})

describe('管理概览', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listUsersMock.mockReset()
  })

  it('并发拉取四个统计（每项 pageSize=1），并显示各角色数量', async () => {
    mockCounts({ ADMIN: 2, TEACHER: 5, STUDENT: 42, DISABLED: 3 })

    const { wrapper } = await mountAt('/admin/dashboard', DashboardView)

    expect(listUsersMock).toHaveBeenCalledTimes(4)
    for (const query of listUsersMock.mock.calls.map((call) => call[0] as AdminUserListQuery)) {
      // 只要 total，不要 200 条账号把页面拖慢（§63 列表必须分页）。
      expect(query.pageSize).toBe(1)
    }
    expect(wrapper.find('[data-testid="count-ADMIN"]').text()).toContain('2')
    expect(wrapper.find('[data-testid="count-TEACHER"]').text()).toContain('5')
    expect(wrapper.find('[data-testid="count-STUDENT"]').text()).toContain('42')
    expect(wrapper.find('[data-testid="count-DISABLED"]').text()).toContain('3')
  })

  it('单个统计失败时降级显示"—"，其余数字照常显示并给出提示', async () => {
    listUsersMock.mockImplementation((query: AdminUserListQuery) => {
      if (query.role === 'STUDENT') {
        return Promise.reject(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
      }
      return Promise.resolve(makeUserListResponse([], { total: 4 }))
    })

    const { wrapper } = await mountAt('/admin/dashboard', DashboardView)

    expect(wrapper.find('[data-testid="count-STUDENT"]').text()).toContain('—')
    expect(wrapper.find('[data-testid="count-TEACHER"]').text()).toContain('4')
    expect(wrapper.find('[data-testid="counts-error"]').text()).toContain('网络连接失败')
  })

  it('提供两个账号管理入口（管理员不参与课堂，§4）', async () => {
    mockCounts({})

    const { wrapper } = await mountAt('/admin/dashboard', DashboardView)

    expect(wrapper.find('[data-testid="entry-users"]').attributes('href')).toBe('/admin/users')
    expect(wrapper.find('[data-testid="entry-user-new"]').attributes('href')).toBe(
      '/admin/users/new',
    )
    // 管理端只有一个业务范围：任何课堂相关入口都不该出现。
    expect(wrapper.text()).not.toContain('/admin/classrooms')
  })
})
