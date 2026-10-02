import { ApiError } from '@classwatch/api-client'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import type { Component } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../../router'
import DashboardView from '../DashboardView.vue'
import LoginView from '../LoginView.vue'
import UserNewView from '../UserNewView.vue'
import UsersView from '../UsersView.vue'

/**
 * 每个 view 都在"真实 URL"下挂载：既是冒烟测试，也验证了 §55 的
 * "页面显示的路径 == 实际 URL"（占位组件渲染的是 useRoute().path）。
 *
 * Phase 1 起这些页面会用到 store（会话、当前用户），因此必须装 pinia；
 * 认证接口统一 mock 成"未登录"，保证视图测试既不打真实网络、也不依赖后端。
 */
vi.mock('../../lib/auth-api.ts', () => ({
  login: vi.fn(),
  fetchCurrentUser: vi.fn().mockRejectedValue(new ApiError({ code: 'AUTH_REQUIRED', status: 401 })),
  logout: vi.fn().mockResolvedValue(undefined),
}))

/**
 * WHY 登录页不在下面这张表里：Phase 1 起它已经是真实表单（不再是 PhasePlaceholder
 * 骨架），没有 path / Phase 标注可断言；它的行为由 login-view.spec.ts 覆盖。
 */
const VIEW_CASES: {
  path: string
  title: string
  phase: string
  component: Component
}[] = [
  { path: '/admin/dashboard', title: '管理概览', phase: 'Phase 2', component: DashboardView },
  { path: '/admin/users', title: '用户管理', phase: 'Phase 2', component: UsersView },
  { path: '/admin/users/new', title: '新建账号', phase: 'Phase 2', component: UserNewView },
]

async function mountAt(path: string, component: Component) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  return mount(component, { global: { plugins: [router] } })
}

describe('管理端页面骨架', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it.each(VIEW_CASES)(
    '$path 渲染标题、路径与 Phase 标注',
    async ({ path, title, phase, component }) => {
      const wrapper = await mountAt(path, component)
      const text = wrapper.text()

      expect(text).toContain(title)
      expect(text).toContain(path)
      expect(text).toContain(`将在 ${phase} 实现`)
    },
  )

  it('登录页渲染账号与密码输入框，且不链接到其他入口（§5）', async () => {
    const wrapper = await mountAt('/admin/login', LoginView)
    const html = wrapper.html()

    expect(wrapper.findAll('input[type="password"]')).toHaveLength(1)
    for (const other of ['/student', '/teacher', '/admin']) {
      if (other === '/admin') continue
      expect(html).not.toContain(`href="${other}`)
    }
  })
})
