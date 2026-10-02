import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeClassroom, makeClassroomRun, makeOpenClassroom } from '../../__tests__/fixtures'
import { formatTimeOfDay } from '../../lib/format'
import { routes } from '../../router'
import { useClassroomsStore } from '../../stores/classrooms'
import ClassroomsView from '../ClassroomsView.vue'

/**
 * 我的课堂列表页测试（§7 / §48 / §49；docs/frontend/teacher.md §2、§5）。
 *
 * mock 掉的是 lib/teacher-classrooms-api（HTTP 边界），store 与视图都是真实实现——
 * 这样"点击 → store 请求 → 界面状态"这条链路整体被测到。
 */
const { listClassroomsMock, openClassroomMock, closeClassroomMock } = vi.hoisted(() => ({
  listClassroomsMock: vi.fn(),
  openClassroomMock: vi.fn(),
  closeClassroomMock: vi.fn(),
}))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: vi.fn(),
  createClassroom: vi.fn(),
  updateClassroom: vi.fn(),
  listClassroomStudents: vi.fn(),
  addClassroomStudents: vi.fn(),
  removeClassroomStudent: vi.fn(),
  openClassroom: openClassroomMock,
  closeClassroom: closeClassroomMock,
}))

const CLOSED = makeClassroom({ id: 'room-closed', name: 'C++ 算法训练' })
const OPEN = makeOpenClassroom({ id: 'room-open', name: '数据结构练习' })

async function mountView(): Promise<{ wrapper: ReturnType<typeof mount>; router: Router }> {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/teacher/classrooms')
  await router.isReady()
  const wrapper = mount(ClassroomsView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('课堂列表页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listClassroomsMock.mockReset().mockResolvedValue([OPEN, CLOSED])
    openClassroomMock.mockReset()
    closeClassroomMock.mockReset()
  })

  it('渲染每个课堂的名称、状态徽章、学生数与"本次开始于"', async () => {
    const { wrapper } = await mountView()

    const openCard = wrapper.find('[data-testid="classroom-card-room-open"]')
    expect(openCard.text()).toContain('数据结构练习')
    expect(openCard.text()).toContain('已开启')
    expect(openCard.text()).toContain('3 人')
    // 时间断言用同一个格式化函数推导，测试因此不依赖运行环境的时区。
    expect(wrapper.find('[data-testid="classroom-run-room-open"]').text()).toContain(
      formatTimeOfDay(OPEN.currentRun?.openedAt),
    )

    const closedCard = wrapper.find('[data-testid="classroom-card-room-closed"]')
    expect(closedCard.text()).toContain('未开启')
    // 未开启的课堂没有"本次开始于"，因为根本没有 currentRun（§7）。
    expect(wrapper.find('[data-testid="classroom-run-room-closed"]').exists()).toBe(false)
  })

  it('开启中的课堂给"关闭课堂"，未开启的给"开启课堂"', async () => {
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="toggle-room-open"]').text()).toContain('关闭课堂')
    expect(wrapper.find('[data-testid="toggle-room-closed"]').text()).toContain('开启课堂')
  })

  it('没有课堂时显示空状态并提供新建入口，而不是一张空列表', async () => {
    listClassroomsMock.mockResolvedValue([])
    const { wrapper } = await mountView()

    const empty = wrapper.find('[data-testid="classrooms-empty"]')
    expect(empty.text()).toContain('还没有课堂')
    expect(empty.find('a').attributes('href')).toContain('/teacher/classrooms/new')
  })

  it('加载失败时显示错误态与重试，而不是"还没有课堂"', async () => {
    listClassroomsMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR' }))
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="classrooms-error"]').text()).toContain('课堂列表加载失败')
    expect(wrapper.find('[data-testid="classrooms-empty"]').exists()).toBe(false)

    listClassroomsMock.mockResolvedValue([CLOSED])
    await wrapper.find('[data-testid="classrooms-error"] button').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="classroom-card-room-closed"]').exists()).toBe(true)
  })

  it('开启课堂：确认之前不发请求，请求进行中重复点击也只发一次', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="toggle-room-closed"]').trigger('click')
    expect(openClassroomMock).not.toHaveBeenCalled()
    // 确认文案必须说清后果：学生可以进入，并且必须共享整个屏幕。
    expect(wrapper.find('[role="dialog"]').text()).toContain('共享整个屏幕')

    // 让请求一直挂着，模拟"确认后按钮还没渲染成禁用"的那一瞬间：双击不能变成两次请求。
    let resolveOpen: (value: unknown) => void = () => undefined
    openClassroomMock.mockReturnValue(
      new Promise((resolve) => {
        resolveOpen = resolve
      }),
    )
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    expect(openClassroomMock).toHaveBeenCalledTimes(1)

    resolveOpen({
      classroom: makeOpenClassroom({ id: CLOSED.id }),
      run: makeClassroomRun({ classroomId: CLOSED.id }),
    })
    await flushPromises()

    expect(openClassroomMock).toHaveBeenCalledWith(CLOSED.id)
  })

  it('开启成功后用返回的 DTO 更新那一行，并给出成功提示', async () => {
    const { wrapper } = await mountView()
    openClassroomMock.mockResolvedValue({
      classroom: makeOpenClassroom({ id: CLOSED.id, name: 'C++ 算法训练' }),
      run: makeClassroomRun({ classroomId: CLOSED.id }),
    })

    await wrapper.find('[data-testid="toggle-room-closed"]').trigger('click')
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="classroom-status-room-closed"]').text()).toContain('已开启')
    expect(wrapper.find('[data-testid="classrooms-notice"]').text()).toContain('已开启')
    // 状态迁移成功后不需要重新拉列表：返回的 DTO 就是新状态（§48 / §49）。
    expect(listClassroomsMock).toHaveBeenCalledTimes(1)
  })

  it('CLASSROOM_ALREADY_OPEN：关掉弹窗、说明真实状态并刷新列表（不是"操作失败"）', async () => {
    const { wrapper } = await mountView()
    openClassroomMock.mockRejectedValue(
      new ApiError({ code: 'CLASSROOM_ALREADY_OPEN', status: 409 }),
    )

    await wrapper.find('[data-testid="toggle-room-closed"]').trigger('click')
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await flushPromises()

    const notice = wrapper.find('[data-testid="classrooms-notice"]')
    expect(notice.text()).toContain('课堂已经是开启状态，列表已刷新。')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    // mount 时一次 + 冲突后刷新一次。
    expect(listClassroomsMock).toHaveBeenCalledTimes(2)
  })

  it('课堂已不存在（404）时也刷新列表，让那一行消失而不是留在页面上', async () => {
    const { wrapper } = await mountView()
    closeClassroomMock.mockRejectedValue(new ApiError({ code: 'CLASSROOM_NOT_FOUND', status: 404 }))

    await wrapper.find('[data-testid="toggle-room-open"]').trigger('click')
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="classrooms-notice"]').text()).toContain('课堂不存在')
    expect(listClassroomsMock).toHaveBeenCalledTimes(2)
  })

  it('普通失败（网络错误）留在弹窗里，不刷新列表也不关闭弹窗', async () => {
    const { wrapper } = await mountView()
    closeClassroomMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR' }))

    await wrapper.find('[data-testid="toggle-room-open"]').trigger('click')
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="toggle-dialog-error"]').text()).toContain('网络')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    expect(listClassroomsMock).toHaveBeenCalledTimes(1)
  })

  it('状态操作进行中会禁用其它课堂的按钮，防止双击产生两次请求', async () => {
    const { wrapper } = await mountView()
    openClassroomMock.mockReturnValue(new Promise(() => undefined))

    await wrapper.find('[data-testid="toggle-room-closed"]').trigger('click')
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="toggle-room-open"]').attributes('disabled')).toBeDefined()
    expect(useClassroomsStore().pendingId, '这一行仍在进行中，按钮必须保持禁用').toBe(CLOSED.id)
  })
})
