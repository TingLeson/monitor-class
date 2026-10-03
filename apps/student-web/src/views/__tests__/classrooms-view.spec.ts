import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenStudentClassroom, makeStudentClassroom } from '../../__tests__/fixtures'
import {
  installFakeRealtimeSocket,
  makeRoomClosedEvent,
  makeRoomOpenedEvent,
} from '../../__tests__/realtime-fixtures'
import { formatTimeOfDay } from '../../lib/format'
import { routes } from '../../router'
import { useRealtimeStore } from '../../stores/realtime'
import ClassroomsView from '../ClassroomsView.vue'

/**
 * 我的课堂列表页测试（§14 / §26；docs/frontend/student.md §2）。
 *
 * mock 掉的是 lib/student-classrooms-api（HTTP 边界），store 与视图都是真实实现——
 * 这样"进入页面 → 请求 → 卡片渲染 → 点击进入"这条链路整体被测到。
 */
const { listClassroomsMock } = vi.hoisted(() => ({ listClassroomsMock: vi.fn() }))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: vi.fn(),
}))

const OPEN = makeOpenStudentClassroom({ id: 'room-open', name: '数据结构练习' })
const CLOSED = makeStudentClassroom({ id: 'room-closed', name: 'C++ 算法训练' })

async function mountView(): Promise<{ wrapper: ReturnType<typeof mount>; router: Router }> {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/student/classrooms')
  await router.isReady()
  const wrapper = mount(ClassroomsView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('我的课堂列表页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listClassroomsMock.mockReset().mockResolvedValue([OPEN, CLOSED])
  })

  it('OPEN 的卡片：课堂名、老师、已开启，以及指向 PreJoin 的「进入课堂」', async () => {
    const { wrapper } = await mountView()

    const card = wrapper.find('[data-testid="classroom-card-room-open"]')
    expect(card.text()).toContain('数据结构练习')
    expect(card.text()).toContain('李老师')
    expect(card.text()).toContain('已开启')

    const enter = card.find('[data-testid="enter-room-open"]')
    expect(enter.element.tagName).toBe('A')
    expect(enter.text()).toBe('进入课堂')
    // 链接必须直接落到这个课堂的 PreJoin，而不是列表页自己或别的课堂。
    expect(enter.attributes('href')).toBe('/student/classrooms/room-open')
  })

  it('CLOSED 的卡片：显示「未开启」而不是消失，按钮禁用并写明「暂不可进入」（§14）', async () => {
    const { wrapper } = await mountView()

    const card = wrapper.find('[data-testid="classroom-card-room-closed"]')
    expect(card.exists()).toBe(true)
    expect(card.text()).toContain('C++ 算法训练')
    expect(card.text()).toContain('王老师')
    expect(card.text()).toContain('未开启')

    const enter = card.find('[data-testid="enter-room-closed"]')
    expect(enter.text()).toBe('暂不可进入')
    // 必须是"可见但禁用"，而不是隐藏，也不是一句报错。
    expect(enter.element.tagName).toBe('BUTTON')
    expect(enter.attributes('disabled')).toBeDefined()
  })

  it('只有 OPEN 且有 currentRun 时才显示「本次开始于 HH:mm」', async () => {
    const { wrapper } = await mountView()

    const run = wrapper.find('[data-testid="classroom-run-room-open"]')
    expect(run.text()).toContain('本次开始于')
    // 时间断言用同一个格式化函数推导，测试因此不依赖运行环境的时区。
    expect(run.text()).toContain(formatTimeOfDay(OPEN.currentRun?.openedAt))

    // 未开启的课堂没有 currentRun，那一行必须不存在（而不是显示成"本次开始于 —"）。
    expect(wrapper.find('[data-testid="classroom-run-room-closed"]').exists()).toBe(false)
  })

  it('页面上不出现任何"人数 / 其他同学"之类的信息（§26）', async () => {
    const { wrapper } = await mountView()

    expect(wrapper.text()).toContain('数据结构练习')
    for (const forbidden of ['人数', '同学', '其他学生', '参与者', '在线']) {
      expect(wrapper.text(), `列表页不得出现「${forbidden}」`).not.toContain(forbidden)
    }
    // 也不允许把 DTO 里根本不存在的字段名渲染出来（说明有人在伪造数据）。
    expect(wrapper.html()).not.toContain('studentCount')
  })

  it('没有任何课堂时给出空状态说明，而不是一张空列表或"注册"之类的入口（§2.2）', async () => {
    listClassroomsMock.mockResolvedValue([])
    const { wrapper } = await mountView()

    const empty = wrapper.find('[data-testid="classrooms-empty"]')
    expect(empty.text()).toContain('还没有被加入任何课堂')
    expect(empty.text()).toContain('老师把你加入课堂后，这里就会出现')
    expect(wrapper.text()).not.toContain('注册')
    expect(wrapper.text()).not.toContain('密码')
  })

  it('加载失败时显示错误态与重试，而不是"还没有被加入任何课堂"', async () => {
    listClassroomsMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const { wrapper } = await mountView()

    const error = wrapper.find('[data-testid="classrooms-error"]')
    expect(error.text()).toContain('课堂列表加载失败')
    expect(error.text()).toContain('网络连接失败')
    expect(wrapper.find('[data-testid="classrooms-empty"]').exists()).toBe(false)

    // 重试必须真的再发一次请求，并把页面带出错误态。
    listClassroomsMock.mockResolvedValue([CLOSED])
    await error.find('button').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="classrooms-error"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="classroom-card-room-closed"]').exists()).toBe(true)
  })

  it('首次加载期间显示占位，不会先渲染出"没有课堂"', async () => {
    listClassroomsMock.mockReturnValue(new Promise(() => undefined))
    const { wrapper } = await mountView()

    expect(wrapper.text()).toContain('正在加载你的课堂')
    expect(wrapper.find('[data-testid="classrooms-empty"]').exists()).toBe(false)
  })

  it('§47/§48：老师开课的实时事件让「进入课堂」当场变可用（不必刷新页面）', async () => {
    const { wrapper } = await mountView()
    // 关闭状态下的按钮是"可见但禁用"（§14），链接才是"真的能进"。
    const before = wrapper
      .find('[data-testid="classroom-card-room-closed"]')
      .find('[data-testid="enter-room-closed"]')
    expect(before.element.tagName).toBe('BUTTON')

    const realtime = useRealtimeStore()
    const socket = installFakeRealtimeSocket()
    realtime.start()
    socket.open()
    socket.emit(makeRoomOpenedEvent({ classroomId: 'room-closed', runId: 'run-9' }))
    await flushPromises()

    const card = wrapper.find('[data-testid="classroom-card-room-closed"]')
    expect(card.text()).toContain('已开启')
    const enter = card.find('[data-testid="enter-room-closed"]')
    expect(enter.exists()).toBe(true)
    expect(enter.attributes('href')).toBe('/student/classrooms/room-closed')
  })

  it('§47/§49：课堂被关闭的实时事件让「进入课堂」当场消失', async () => {
    const { wrapper } = await mountView()
    expect(wrapper.find('[data-testid="enter-room-open"]').exists()).toBe(true)

    const realtime = useRealtimeStore()
    const socket = installFakeRealtimeSocket()
    realtime.start()
    socket.open()
    socket.emit(makeRoomClosedEvent({ classroomId: 'room-open' }))
    await flushPromises()

    const card = wrapper.find('[data-testid="classroom-card-room-open"]')
    expect(card.text()).toContain('未开启')
    // 变回禁用按钮（而不是消失）：课堂仍然在列表里，只是现在进不去。
    const enter = card.find('[data-testid="enter-room-open"]')
    expect(enter.element.tagName).toBe('BUTTON')
    expect(enter.text()).toBe('暂不可进入')
  })

  it('§47：实时通道没连上时说一句，连上后这句话消失（连不上不能假装一切正常）', async () => {
    const { wrapper } = await mountView()
    const hint = wrapper.find('[data-testid="classrooms-realtime"]')
    // 还没启动通道：不该出现任何"连接失败"的噪音。
    expect(hint.exists()).toBe(false)

    const realtime = useRealtimeStore()
    const socket = installFakeRealtimeSocket()
    realtime.start()
    await flushPromises()
    expect(wrapper.find('[data-testid="classrooms-realtime"]').text()).toContain('正在连接')

    socket.open()
    await flushPromises()
    expect(wrapper.find('[data-testid="classrooms-realtime"]').exists()).toBe(false)
  })
})
