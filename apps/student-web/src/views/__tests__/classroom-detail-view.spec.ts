import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenStudentClassroom, makeStudentClassroom } from '../../__tests__/fixtures'
import { routes } from '../../router'
import { useClassroomsStore } from '../../stores/classrooms'
import ClassroomDetailView from '../ClassroomDetailView.vue'

/**
 * PreJoin 页测试（§15 Step 2 / §16 / §57 / §70；docs/frontend/student.md §3）。
 *
 * 这一页最重要的两条断言是"反面"的：
 * 1. §57 的隐私告知必须是**原文**——学生正是靠它才知道老师会看到整块屏幕上的
 *    一切，改一个字都是对这条告知的削弱；
 * 2. 主按钮必须是**禁用**的。Phase 4 里整屏共享还没有实现（§70），
 *    一个"看起来能点"的按钮会让学生以为系统坏了。
 */
const { getClassroomMock } = vi.hoisted(() => ({ getClassroomMock: vi.fn() }))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: vi.fn(),
  getClassroom: getClassroomMock,
}))

/** §57 原文（任务书与 docs/frontend/student.md §3.1 完全一致）。 */
const PRIVACY_NOTICE =
  '进入课堂后，老师能够看到当前共享显示器上的内容，包括你在其他应用程序和浏览器中打开的内容。' +
  '请关闭与课堂无关的隐私信息后再继续。'

const OPEN = makeOpenStudentClassroom({ id: 'room-open', name: 'C++ 算法训练' })
const CLOSED = makeStudentClassroom({ id: 'room-closed', name: '数据结构练习' })

/** 在真实 URL 下挂载 PreJoin 页：视图通过 useRoute().params.id 取课堂 id。 */
async function mountView(id: string) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(`/student/classrooms/${id}`)
  await router.isReady()
  const wrapper = mount(ClassroomDetailView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('PreJoin 页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    getClassroomMock.mockReset().mockResolvedValue(OPEN)
  })

  it('顶部显示课堂名、说明副标题、老师与状态', async () => {
    const { wrapper } = await mountView('room-open')

    expect(wrapper.find('[data-testid="classroom-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="classroom-description"]').text()).toBe('第三章 动态规划')
    expect(wrapper.find('[data-testid="classroom-teacher"]').text()).toBe('李老师')
    expect(wrapper.find('[data-testid="classroom-status"]').text()).toContain('已开启')
    expect(getClassroomMock).toHaveBeenCalledWith('room-open')
  })

  it('没有说明时不渲染空的副标题行', async () => {
    getClassroomMock.mockResolvedValue(makeOpenStudentClassroom({ description: null }))
    const { wrapper } = await mountView('room-open')

    expect(wrapper.find('[data-testid="classroom-description"]').exists()).toBe(false)
  })

  it('明确列出「必须：整个显示器」与「可选：摄像头 / 麦克风」', async () => {
    const { wrapper } = await mountView('room-open')

    const required = wrapper.find('[data-testid="prejoin-required"]')
    expect(required.text()).toContain('必须')
    expect(required.text()).toContain('整个显示器')
    // 只说"共享屏幕"不够：学生必须知道那是整块显示器，而不是某个窗口。
    expect(required.text()).toContain('整块屏幕')

    const optional = wrapper.find('[data-testid="prejoin-optional"]')
    expect(optional.text()).toContain('可选')
    expect(optional.text()).toContain('摄像头')
    expect(optional.text()).toContain('麦克风')
    // 可选必须写清楚"不影响进入课堂"，否则学生会以为不授权就进不去（§24/§25）。
    expect(optional.text()).toContain('不影响进入课堂')

    // 可选设备搞错分组（混进"必须"）会让整页的告知失真。
    expect(required.text()).not.toContain('摄像头')
    expect(required.text()).not.toContain('麦克风')
  })

  it('§57 隐私告知原文出现（不得改写、不得省略）', async () => {
    const { wrapper } = await mountView('room-open')

    const notice = wrapper.find('[data-testid="privacy-notice"]')
    expect(notice.exists()).toBe(true)
    expect(notice.text().trim()).toBe(PRIVACY_NOTICE)
  })

  it('主按钮是「共享整个屏幕并进入课堂」且禁用，并说明整屏共享属于 Phase 5', async () => {
    const { wrapper } = await mountView('room-open')

    const button = wrapper.find('[data-testid="enter-classroom"]')
    expect(button.text()).toBe('共享整个屏幕并进入课堂')
    expect(button.attributes('disabled')).toBeDefined()

    // 说明必须是信息样式（不是报错），并且明确指向 Phase 5，而不是含糊的"暂不可用"。
    const notice = wrapper.find('[data-testid="phase-notice"]')
    expect(notice.text()).toContain('整屏共享与进入课堂将在 Phase 5 接入')
    expect(notice.classes()).not.toContain('text-status-danger')
  })

  it('Phase 4 不发起任何屏幕共享请求：页面里没有 getDisplayMedia 之类的调用', async () => {
    const getDisplayMedia = vi.fn()
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getDisplayMedia },
    })

    try {
      const { wrapper } = await mountView('room-open')
      await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
      await flushPromises()

      expect(getDisplayMedia).not.toHaveBeenCalled()
    } finally {
      Reflect.deleteProperty(navigator, 'mediaDevices')
    }
  })

  it('CLOSED：显示「老师尚未开启本课堂」并保留回列表的出口，主按钮依然禁用', async () => {
    getClassroomMock.mockResolvedValue(CLOSED)
    const { wrapper } = await mountView('room-closed')

    expect(wrapper.find('[data-testid="classroom-status"]').text()).toContain('未开启')
    expect(wrapper.find('[data-testid="classroom-closed-notice"]').text()).toContain(
      '老师尚未开启本课堂',
    )
    expect(wrapper.find('[data-testid="enter-classroom"]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="back-to-classrooms"]').attributes('href')).toBe(
      '/student/classrooms',
    )
  })

  it('404 STUDENT_NOT_ASSIGNED：说明不在名单里 + 回列表入口，不给"重试"', async () => {
    getClassroomMock.mockRejectedValue(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 }))
    const { wrapper } = await mountView('room-x')

    const state = wrapper.find('[data-testid="classroom-not-assigned"]')
    expect(state.text()).toContain('你不在这个课堂的名单里')
    expect(state.text()).toContain('请联系老师确认')
    expect(wrapper.find('[data-testid="back-to-classrooms"]').attributes('href')).toBe(
      '/student/classrooms',
    )
    // 停在这一页反复重试是没有意义的：授权不会因为重试而改变。
    expect(wrapper.find('[data-testid="classroom-detail-error"]').exists()).toBe(false)
    expect(state.find('button').exists()).toBe(false)
    // 也不能显示成"课堂不存在"——那会让未被授权的学生以为自己猜错了 id（§63）。
    expect(wrapper.text()).not.toContain('课堂不存在')
  })

  it('网络失败：显示错误文案与重试，重试成功后渲染课堂', async () => {
    getClassroomMock.mockRejectedValueOnce(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const { wrapper } = await mountView('room-open')

    const error = wrapper.find('[data-testid="classroom-detail-error"]')
    expect(error.text()).toContain('网络连接失败')

    getClassroomMock.mockResolvedValue(OPEN)
    await error.find('button').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="classroom-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="classroom-detail-error"]').exists()).toBe(false)
  })

  it('切换 :id 时先清空：新课堂的数据到达前，页面上不会留着上一个课堂的名字', async () => {
    const A = makeOpenStudentClassroom({ id: 'room-a', name: 'A 课堂' })
    const B = makeStudentClassroom({ id: 'room-b', name: 'B 课堂', status: 'OPEN' })
    // B 的响应永远不来：要验证的正是"在 B 的数据到达之前，页面上已经没有 A 了"。
    getClassroomMock.mockImplementation((id: string) => {
      if (id === A.id) return Promise.resolve(A)
      if (id === B.id) return new Promise(() => undefined)
      return Promise.reject(new Error(`未预期的课堂 id：${id}`))
    })
    const { wrapper, router } = await mountView('room-a')
    expect(wrapper.find('[data-testid="classroom-name"]').text()).toBe('A 课堂')

    await router.push('/student/classrooms/room-b')
    await flushPromises()

    expect(wrapper.text()).not.toContain('A 课堂')
    expect(wrapper.text()).toContain('正在加载课堂')
  })

  it('从已授权的课堂切到不在名单里的课堂：显示未授权说明，不残留上一个课堂', async () => {
    const A = makeOpenStudentClassroom({ id: 'room-a', name: 'A 课堂' })
    getClassroomMock.mockImplementation((id: string) =>
      id === 'room-a'
        ? Promise.resolve(A)
        : Promise.reject(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 })),
    )
    const { wrapper, router } = await mountView('room-a')

    await router.push('/student/classrooms/room-x')
    await flushPromises()

    expect(wrapper.text()).not.toContain('A 课堂')
    expect(wrapper.find('[data-testid="classroom-not-assigned"]').exists()).toBe(true)
  })

  it('离开页面时清空 store 里的详情（下次进入不会闪现上一个课堂）', async () => {
    const { wrapper } = await mountView('room-open')
    const store = useClassroomsStore()
    expect(store.current?.id).toBe('room-open')

    wrapper.unmount()

    expect(store.current).toBeNull()
    expect(store.currentError).toBeNull()
  })

  it('数据到达前不渲染空白页，而是显示加载占位', async () => {
    getClassroomMock.mockReturnValue(new Promise(() => undefined))
    const { wrapper } = await mountView('room-open')

    expect(wrapper.text()).toContain('正在加载课堂')
    expect(wrapper.find('[data-testid="classroom-name"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="classroom-not-assigned"]').exists()).toBe(false)
  })
})
