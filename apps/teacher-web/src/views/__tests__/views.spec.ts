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
 * Phase 6 之后教师端**没有**占位页了：监督墙也是真实实现（§29/§51/§52）。
 */
vi.mock('../../lib/auth-api.ts', () => ({
  login: vi.fn(),
  fetchCurrentUser: vi.fn().mockRejectedValue(new ApiError({ code: 'AUTH_REQUIRED', status: 401 })),
  logout: vi.fn().mockResolvedValue(undefined),
}))

/**
 * 监督接口：这一页现在会真的申请媒体凭据并拉 monitor 数据。
 * 页面级的冒烟测试只关心"渲染出内容"，媒体链路的行为由
 * `classroom-monitor-view.spec.ts` 覆盖，这里给一个不产生副作用的空列表。
 */
vi.mock('../../lib/teacher-monitor-api.ts', () => ({
  getMonitor: vi.fn().mockResolvedValue([]),
  requestMediaToken: vi
    .fn()
    .mockResolvedValue({ livekitUrl: 'wss://example.invalid', token: 'test-token' }),
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

  it('Phase 6：监督墙不再是占位页，而是真的渲染课堂与学生卡片容器', async () => {
    const wrapper = await mountAt('/teacher/classrooms/room-1/monitor', ClassroomMonitorView)

    const text = wrapper.text()
    expect(text).toContain('课堂监督墙')
    expect(text).toContain('C++ 算法训练')
    expect(wrapper.findComponent(PhasePlaceholder).exists()).toBe(false)
  })

  it('监督墙给出回到课堂详情的入口（不用按浏览器后退）', async () => {
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
