import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { PhasePlaceholder } from '@classwatch/ui'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import type { Component } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeClassroom, makeOpenClassroom } from '../../__tests__/fixtures'
import { routes } from '../../router'
import ClassroomDetailView from '../ClassroomDetailView.vue'
import ClassroomMonitorView from '../ClassroomMonitorView.vue'
import ClassroomNewView from '../ClassroomNewView.vue'
import ClassroomsView from '../ClassroomsView.vue'
import DashboardView from '../DashboardView.vue'
import LoginView from '../LoginView.vue'

/**
 * 页面级冒烟测试：每个 view 都在**真实 URL**下挂载一次。
 *
 * 既验证 §55 的路径契约，也钉住"哪些页面已经是真实实现、哪些仍是占位"——
 * Phase 3 结束后只有监督墙还是占位（它属于 Phase 7），其余页面必须真的渲染数据。
 */
vi.mock('../../lib/auth-api.ts', () => ({
  login: vi.fn(),
  fetchCurrentUser: vi.fn().mockRejectedValue(new ApiError({ code: 'AUTH_REQUIRED', status: 401 })),
  logout: vi.fn().mockResolvedValue(undefined),
}))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: vi
    .fn()
    .mockResolvedValue([
      makeOpenClassroom({ id: 'room-open', name: '数据结构练习' }),
      makeClassroom({ id: 'room-closed', name: 'C++ 算法训练' }),
    ]),
  getClassroom: vi.fn().mockResolvedValue(makeClassroom({ id: 'room-1', name: 'C++ 算法训练' })),
  createClassroom: vi.fn(),
  updateClassroom: vi.fn(),
  listClassroomStudents: vi.fn().mockResolvedValue({ students: [] }),
  addClassroomStudents: vi.fn(),
  removeClassroomStudent: vi.fn(),
  openClassroom: vi.fn(),
  closeClassroom: vi.fn(),
}))

async function mountAt(path: string, component: Component) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(component, { global: { plugins: [router] } })
  await flushPromises()
  return wrapper
}

describe('教师端页面', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it.each([
    ['/teacher/dashboard', DashboardView, '我的课堂'],
    ['/teacher/classrooms', ClassroomsView, '数据结构练习'],
    ['/teacher/classrooms/new', ClassroomNewView, '新建课堂'],
    ['/teacher/classrooms/room-1', ClassroomDetailView, 'C++ 算法训练'],
  ] as const)('%s 渲染真实内容（不再是 Phase 占位页）', async (path, component, expected) => {
    const wrapper = await mountAt(path, component)

    expect(wrapper.text()).toContain(expected)
    expect(wrapper.findComponent(PhasePlaceholder).exists()).toBe(false)
  })

  it('/teacher/classrooms/:id/monitor 仍是占位页，写明 Phase 7 与监督墙范围', async () => {
    const wrapper = await mountAt('/teacher/classrooms/room-1/monitor', ClassroomMonitorView)

    const text = wrapper.text()
    expect(text).toContain('/teacher/classrooms/room-1/monitor')
    expect(text).toContain('课堂监督墙')
    expect(text).toContain('将在 Phase 7：多学生监督墙与 Focus View 实现')
    expect(text).toContain('Focus View')
  })

  it('监督墙占位页给出回到课堂详情的入口（不用按浏览器后退）', async () => {
    const wrapper = await mountAt('/teacher/classrooms/room-1/monitor', ClassroomMonitorView)

    expect(wrapper.find('[data-testid="monitor-back"]').attributes('href')).toBe(
      '/teacher/classrooms/room-1',
    )
  })

  it('登录页渲染账号与密码输入框，且不链接到其他入口（§5）', async () => {
    const wrapper = await mountAt('/teacher/login', LoginView)
    const html = wrapper.html()

    expect(wrapper.findAll('input[type="password"]')).toHaveLength(1)
    for (const other of ['/student', '/admin']) {
      expect(html).not.toContain(`href="${other}`)
    }
  })
})
