import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import type { Component } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenStudentClassroom } from '../../__tests__/fixtures'
import { routes } from '../../router'
import ClassroomDetailView from '../ClassroomDetailView.vue'
import ClassroomsView from '../ClassroomsView.vue'
import LoginView from '../LoginView.vue'
import SessionView from '../SessionView.vue'

/**
 * 四个页面在各自**真实 URL** 下的冒烟测试（§55）。
 *
 * WHY 还留着这张表：路由表、`useRoute()` 的用法、store 的装配方式三者任意一处
 * 走偏，最典型的症状就是"页面在真实 URL 下渲染不出内容"。这里不断言业务细节
 * （那些在各自的 spec 里），只保证"这一页在自己的地址上真的能打开、并渲染出
 * 它该有的东西"。
 *
 * 认证接口统一 mock 成"未登录"，课堂接口 mock 成固定数据：视图测试既不打真实
 * 网络，也不依赖后端是否就绪。
 */
vi.mock('../../lib/auth-api.ts', () => ({
  login: vi.fn(),
  fetchCurrentUser: vi.fn().mockRejectedValue(new ApiError({ code: 'AUTH_REQUIRED', status: 401 })),
  logout: vi.fn().mockResolvedValue(undefined),
}))

const { listClassroomsMock, getClassroomMock } = vi.hoisted(() => ({
  listClassroomsMock: vi.fn(),
  getClassroomMock: vi.fn(),
}))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: getClassroomMock,
}))

const OPEN = makeOpenStudentClassroom({ id: 'room-open', name: '数据结构练习' })

const VIEW_CASES: {
  path: string
  component: Component
  /** 这一页在自己的 URL 下必须渲染出来的内容。 */
  expected: string[]
}[] = [
  {
    path: '/student/classrooms',
    component: ClassroomsView,
    expected: ['我的课堂', '数据结构练习', '进入课堂'],
  },
  {
    path: '/student/classrooms/room-open',
    component: ClassroomDetailView,
    expected: ['数据结构练习', '共享整个屏幕并进入课堂'],
  },
  {
    path: '/student/session/session-1',
    component: SessionView,
    // 课堂会话仍是 Phase 6 的占位页：路径与 Phase 标注都要如实显示。
    expected: ['/student/session/session-1', '课堂会话 · Phase 6 起实现', '将在 Phase 6 实现'],
  },
]

async function mountAt(path: string, component: Component) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(component, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('学生端页面骨架', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listClassroomsMock.mockReset().mockResolvedValue([OPEN])
    getClassroomMock.mockReset().mockResolvedValue(OPEN)
  })

  it.each(VIEW_CASES)(
    '$path 能在自己的 URL 下渲染出内容',
    async ({ path, component, expected }) => {
      const { wrapper, router } = await mountAt(path, component)

      expect(router.currentRoute.value.path).toBe(path)
      for (const text of expected) {
        expect(wrapper.text(), `页面缺少「${text}」`).toContain(text)
      }
    },
  )

  it('登录页渲染账号表单，且没有任何指向其他入口的链接（§5）', async () => {
    const { wrapper } = await mountAt('/student/login', LoginView)
    const html = wrapper.html()

    expect(wrapper.find('input').exists()).toBe(true)
    expect(html).not.toContain('href="/teacher')
    expect(html).not.toContain('href="/admin')
  })
})
