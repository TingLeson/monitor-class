import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeClassroom, makeOpenClassroom } from '../../__tests__/fixtures'
import { routes } from '../../router'
import DashboardView from '../DashboardView.vue'

/**
 * 工作台概览测试（§55 Teacher；docs/frontend/teacher.md §1）。
 *
 * 概览数字必须由课堂列表推导（不新增统计接口），失败时降级为破折号——
 * 用 0 兜底会让"读不到数据"看起来像"你没有开启中的课堂"。
 */
const { listClassroomsMock } = vi.hoisted(() => ({ listClassroomsMock: vi.fn() }))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: vi.fn(),
  createClassroom: vi.fn(),
  updateClassroom: vi.fn(),
  listClassroomStudents: vi.fn(),
  addClassroomStudents: vi.fn(),
  removeClassroomStudent: vi.fn(),
  openClassroom: vi.fn(),
  closeClassroom: vi.fn(),
}))

async function mountView() {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/teacher/dashboard')
  await router.isReady()
  const wrapper = mount(DashboardView, { global: { plugins: [router] } })
  await flushPromises()
  return wrapper
}

describe('工作台概览', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listClassroomsMock
      .mockReset()
      .mockResolvedValue([
        makeOpenClassroom({ id: 'room-open' }),
        makeClassroom({ id: 'room-closed-1' }),
        makeClassroom({ id: 'room-closed-2' }),
      ])
  })

  it('显示课堂总数与开启中数量（由列表推导）', async () => {
    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="stat-total"]').text()).toBe('3')
    expect(wrapper.find('[data-testid="stat-open"]').text()).toBe('1')
  })

  it('开启中为 0 时显示 0（这是一个明确的事实，不是"读不到"）', async () => {
    listClassroomsMock.mockResolvedValue([makeClassroom()])
    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="stat-open"]').text()).toBe('0')
  })

  it('加载失败时降级为破折号并给出重试，而不是假装是 0', async () => {
    listClassroomsMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR' }))
    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="stat-total"]').text()).toBe('—')
    expect(wrapper.find('[data-testid="stat-open"]').text()).toBe('—')
    expect(wrapper.find('[data-testid="dashboard-error"]').text()).toContain('网络')
  })

  it('提供常用入口：我的课堂与新建课堂', async () => {
    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="dashboard-classrooms-link"]').attributes('href')).toBe(
      '/teacher/classrooms',
    )
    expect(wrapper.find('[data-testid="dashboard-new-link"]').attributes('href')).toBe(
      '/teacher/classrooms/new',
    )
  })
})
