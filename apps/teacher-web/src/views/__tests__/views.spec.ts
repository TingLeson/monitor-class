import { mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import type { Component } from 'vue'
import { describe, expect, it } from 'vitest'
import { routes } from '../../router'
import ClassroomDetailView from '../ClassroomDetailView.vue'
import ClassroomMonitorView from '../ClassroomMonitorView.vue'
import ClassroomNewView from '../ClassroomNewView.vue'
import ClassroomsView from '../ClassroomsView.vue'
import DashboardView from '../DashboardView.vue'
import LoginView from '../LoginView.vue'

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
  { path: '/teacher/login', title: '教师登录', phase: 'Phase 1', component: LoginView },
  { path: '/teacher/dashboard', title: '工作台', phase: 'Phase 3', component: DashboardView },
  {
    path: '/teacher/classrooms',
    title: '我的课堂',
    phase: 'Phase 3',
    component: ClassroomsView,
  },
  {
    path: '/teacher/classrooms/new',
    title: '新建课堂',
    phase: 'Phase 3',
    component: ClassroomNewView,
  },
  {
    path: '/teacher/classrooms/room-1',
    title: '课堂详情',
    phase: 'Phase 3',
    component: ClassroomDetailView,
  },
  {
    path: '/teacher/classrooms/room-1/monitor',
    title: '课堂监督台',
    phase: 'Phase 7',
    component: ClassroomMonitorView,
  },
]

async function mountAt(path: string, component: Component) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  return mount(component, { global: { plugins: [router] } })
}

describe('教师端页面骨架', () => {
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
    const wrapper = await mountAt('/teacher/login', LoginView)
    const html = wrapper.html()

    expect(html).not.toContain('href="/student')
    expect(html).not.toContain('href="/admin')
  })
})
