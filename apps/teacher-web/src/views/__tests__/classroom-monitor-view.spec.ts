import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenClassroom } from '../../__tests__/fixtures.ts'
import {
  installFakeMonitorRoom,
  makeMonitorStudent,
  makeOfflineStudent,
  makeScreenLostStudent,
} from '../../__tests__/monitor-fixtures.ts'
import { routes } from '../../router'
import ClassroomMonitorView from '../ClassroomMonitorView.vue'

/**
 * 监督墙页面测试（§29 / §51 / §52）。
 *
 * 这一页存在的唯一理由是"老师真的能看到学生桌面"，所以断言分成两组：
 *
 * 1. **卡片由 Monitor DTO 渲染**（业务状态，§51）：名字、三种徽章、四种主体文案；
 * 2. **画面由 LiveKit 订阅提供**（媒体状态）：只有订阅到位的卡片才有 `<video>`，
 *    而且它必须 `autoplay + playsinline + muted`——少一个 `muted`，老师一开口
 *    就会把学生桌面的声音放出来形成回声。
 *
 * 反面断言同样重要：媒体连不上时**不能白屏**，业务状态照样要显示出来。
 */

const { getMonitorMock, requestMediaTokenMock, getClassroomMock } = vi.hoisted(() => ({
  getMonitorMock: vi.fn(),
  requestMediaTokenMock: vi.fn(),
  getClassroomMock: vi.fn(),
}))

vi.mock('../../lib/teacher-monitor-api.ts', () => ({
  getMonitor: getMonitorMock,
  requestMediaToken: requestMediaTokenMock,
}))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: vi.fn().mockResolvedValue([]),
  getClassroom: getClassroomMock,
  createClassroom: vi.fn(),
  updateClassroom: vi.fn(),
  listClassroomStudents: vi.fn().mockResolvedValue({ students: [] }),
  addClassroomStudents: vi.fn(),
  removeClassroomStudent: vi.fn(),
  openClassroom: vi.fn(),
  closeClassroom: vi.fn(),
}))

const CLASSROOM = makeOpenClassroom({ id: 'room-1', name: 'C++ 算法训练', studentCount: 2 })

async function mountView() {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/teacher/classrooms/room-1/monitor')
  await router.isReady()
  const wrapper = mount(ClassroomMonitorView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('课堂监督墙', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    getMonitorMock.mockReset().mockResolvedValue([makeMonitorStudent()])
    requestMediaTokenMock.mockReset().mockResolvedValue({
      livekitUrl: 'wss://classwatch-test.livekit.cloud',
      token: 'teacher-token',
    })
    getClassroomMock.mockReset().mockResolvedValue(CLASSROOM)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  it('页头显示课堂名、状态与"正在监督 N 路画面"', async () => {
    installFakeMonitorRoom()
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="monitor-title"]').text()).toBe('课堂监督墙')
    expect(wrapper.find('[data-testid="monitor-classroom-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="monitor-classroom-status"]').text()).toContain('已开启')
    expect(wrapper.find('[data-testid="monitor-subscribed-count"]').text()).toContain('1 路画面')
  })

  it('按 Monitor DTO 渲染卡片：名字 + 🟢 正常 + 画面', async () => {
    const harness = installFakeMonitorRoom()
    const { wrapper } = await mountView()

    const tiles = wrapper.findAll('[data-testid="monitor-tile"]')
    expect(tiles).toHaveLength(1)
    expect(tiles[0]?.find('[data-testid="tile-name"]').text()).toBe('张三')
    expect(tiles[0]?.find('[data-testid="tile-badge"]').text()).toContain('🟢 正常')
    // 屏幕轨道已经订阅并 attach 到 <video>。
    expect(tiles[0]?.find('[data-testid="tile-video"]').exists()).toBe(true)
    expect(harness.current().attachedElements).toHaveLength(1)
  })

  it('§29：<video> 必须 autoplay + playsinline + muted（少一个 muted 就会回声）', async () => {
    installFakeMonitorRoom()
    const { wrapper } = await mountView()

    const video = wrapper.find('[data-testid="tile-video"]')
    const element = video.element as HTMLVideoElement
    expect(element.autoplay).toBe(true)
    expect(element.muted).toBe(true)
    // playsinline 走 HTML 属性（浏览器会把同名 attribute 映射到 playsInline 属性；
    // happy-dom 不映射，所以这里断言 attribute 本身）。
    expect(video.attributes('playsinline')).toBeDefined()
    // 屏幕轨道没有音频，但"显式静音"是一条必须写死的约束，不能依赖默认值。
    expect(video.attributes('muted')).not.toBe('false')
    expect(video.attributes('autoplay')).not.toBe('false')
  })

  it('三种徽章各自渲染正确（正常 / 屏幕中断 / 未连接）', async () => {
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', displayName: '张三', sessionId: 'session-a' }),
      makeScreenLostStudent({ displayName: '李四', sessionId: 'session-lost' }),
      makeOfflineStudent({ displayName: '王五' }),
    ])
    // 媒体层里只有"正常"的那一个（session-a）；另两个本来就不该被订阅。
    installFakeMonitorRoom({ participants: ['session-a'] })
    const { wrapper } = await mountView()

    const badges = wrapper.findAll('[data-testid="tile-badge"]').map((node) => node.text())
    expect(badges[0]).toContain('🟢 正常')
    expect(badges[1]).toContain('🔴 屏幕中断')
    expect(badges[2]).toContain('⚪ 未连接')

    // 只有"正常"的那张有画面；另两张给出各自的原因，而不是一个黑框。
    expect(wrapper.findAll('[data-testid="tile-video"]')).toHaveLength(1)
    const placeholders = wrapper.findAll('[data-testid="tile-placeholder"]').map((n) => n.text())
    expect(placeholders).toEqual(['等待共享', '未连接'])
  })

  it('业务说该共享、媒体还没到时显示"正在订阅画面…"（而不是假装已连接）', async () => {
    const harness = installFakeMonitorRoom()
    harness.flags.subscribeError = new Error('subscribe rejected')
    const { wrapper } = await mountView()

    const placeholder = wrapper.find('[data-testid="tile-placeholder"]')
    expect(placeholder.text()).toBe('画面订阅失败')
    expect(wrapper.find('[data-testid="tile-video"]').exists()).toBe(false)
  })

  it('§52：订阅失败不会重复轰炸（每 10 秒刷新也不会重复订阅同一个 participant）', async () => {
    const harness = installFakeMonitorRoom()
    const { wrapper } = await mountView()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])

    // 模拟两次轮询刷新：卡片重渲染，但订阅调用次数不变。
    getMonitorMock.mockResolvedValue([makeMonitorStudent()])
    await wrapper.vm.$nextTick()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
  })

  it('媒体连接失败：显示明确提示 + 重试入口，同时业务状态照常渲染（不白屏）', async () => {
    const harness = installFakeMonitorRoom({ connectError: new Error('signal failed') })
    const { wrapper } = await mountView()

    const error = wrapper.find('[data-testid="monitor-media-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain('无法连接课堂的媒体服务器')
    // §51：媒体连不上，也要能看到"谁在上课"。
    expect(wrapper.findAll('[data-testid="monitor-tile"]')).toHaveLength(1)
    expect(wrapper.find('[data-testid="monitor-empty"]').exists()).toBe(false)

    harness.flags.connectError = null
    await wrapper.find('[data-testid="monitor-media-retry"]').trigger('click')
    await flushPromises()

    expect(requestMediaTokenMock).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="monitor-media-error"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="tile-video"]').exists()).toBe(true)
  })

  it('monitor 数据失败：给提示与重新加载，已有画面保留', async () => {
    installFakeMonitorRoom()
    getMonitorMock.mockRejectedValueOnce(new Error('boom'))
    const { wrapper } = await mountView()

    const error = wrapper.find('[data-testid="monitor-data-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain('监督数据没有刷新')

    getMonitorMock.mockResolvedValue([makeMonitorStudent()])
    await error.find('button').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="monitor-data-error"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="monitor-tile"]')).toHaveLength(1)
  })

  it('课堂里没有学生：显示空状态，而不是一片空白', async () => {
    installFakeMonitorRoom()
    getMonitorMock.mockResolvedValue([])
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="monitor-empty"]').text()).toContain('还没有学生')
    expect(wrapper.find('[data-testid="monitor-grid"]').exists()).toBe(false)
  })

  it('回到课堂详情的入口一直在（不用按浏览器后退）', async () => {
    installFakeMonitorRoom()
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="monitor-back"]').attributes('href')).toBe(
      '/teacher/classrooms/room-1',
    )
  })

  it('§44：页面里不出现媒体凭据', async () => {
    installFakeMonitorRoom()
    const { wrapper } = await mountView()

    expect(wrapper.html()).not.toContain('teacher-token')
    expect(wrapper.html()).not.toContain('wss://')
  })
})
