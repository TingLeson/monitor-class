import { mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import type { Component } from 'vue'
import { describe, expect, it } from 'vitest'
import { routes } from '../../router'
import DashboardView from '../DashboardView.vue'
import LoginView from '../LoginView.vue'
import UserNewView from '../UserNewView.vue'
import UsersView from '../UsersView.vue'

/**
 * 每个 view 都在"真实 URL"下挂载：既是冒烟测试，也验证了 §55 的
 * "页面显示的路径 == 实际 URL"（占位组件渲染的是 useRoute().path）。
 */
const VIEW_CASES: {
  path: string
  title: string
  phase: string
  component: Component
}[] = [
  { path: '/admin/login', title: '管理员登录', phase: 'Phase 1', component: LoginView },
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

  it('登录页不渲染指向其他入口的链接（三个入口物理分离，§5）', async () => {
    const wrapper = await mountAt('/admin/login', LoginView)
    const html = wrapper.html()

    expect(html).not.toContain('href="/student')
    expect(html).not.toContain('href="/teacher')
  })
})
