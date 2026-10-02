import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeJoinResponse, installFakePublisherRoom } from '../../__tests__/media-fixtures'
import { makeOpenStudentClassroom, makeStudentClassroom } from '../../__tests__/fixtures'
import {
  makeMediaDevicesStub,
  installMediaDevices,
  type MediaDevicesStub,
  type FakeTrack,
} from '../../__tests__/screen-fixtures'
import { CLASSROOM_STATUS_POLL_MS } from '../../lib/media-session-state.ts'
import { requestEntireScreen, type ScreenCapture } from '../../lib/screen-capture'
import { routes } from '../../router'
import SessionView from '../SessionView.vue'

/**
 * 课堂会话页测试（§20 / §22 / §49 / §56）。
 *
 * 这一页里有几条断言是**反面**的，而且它们比正面断言更重要：
 *
 * 1. 页面上**没有**任何 `<video>`（§56：不显示自己的屏幕预览，避免 screen-in-screen）；
 * 2. 摄像头 / 麦克风**不是可点的开关**（Phase 9/10 才接入，假开关会让学生以为坏了）；
 * 3. 重新共享**不重新 join**（会话还在，只是轨道没了）；
 * 4. 重新共享必须**重新过完整 Gate**——这里刻意**不** mock `requestEntireScreen`，
 *    用真实的 Gate + 假浏览器（mediaDevices 替身）跑，这样"选了窗口会被拒绝"
 *    这条 §16 的核心不变量在会话页这条恢复路径上也被真的验证一次。
 */

const { joinClassroomMock, leaveSessionMock, getClassroomMock } = vi.hoisted(() => ({
  joinClassroomMock: vi.fn(),
  leaveSessionMock: vi.fn(),
  getClassroomMock: vi.fn(),
}))

vi.mock('../../lib/student-sessions-api.ts', () => ({
  joinClassroom: joinClassroomMock,
  leaveSession: leaveSessionMock,
}))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: vi.fn(),
  getClassroom: getClassroomMock,
}))

const OPEN = makeOpenStudentClassroom({ id: 'room-open', name: 'C++ 算法训练' })

/** 装一个假浏览器，并真的走一遍 §16 的 Gate，拿到一条合法的屏幕捕获。 */
async function gateCapture(surface: 'monitor' | 'window' = 'monitor'): Promise<{
  devices: MediaDevicesStub
  capture: ScreenCapture
}> {
  const devices = makeMediaDevicesStub(surface)
  installMediaDevices(devices)
  const capture = await requestEntireScreen()
  return { devices, capture }
}

/** 把 PreJoin 交接的结果放进 store，并把会话页挂到对应 URL 上。 */
async function mountSession(options: { prepare?: boolean } = {}) {
  const { useMediaSessionStore } = await import('../../stores/media-session.ts')
  const store = useMediaSessionStore()
  const { devices, capture } = await gateCapture()
  if (options.prepare !== false) {
    store.prepare({
      sessionId: 'session-1',
      classroomId: 'room-open',
      credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'fake-token' },
      capture,
    })
  }

  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/student/session/session-1')
  await router.isReady()
  const wrapper = mount(SessionView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router, store, devices, capture }
}

/** 会话页里那条被 publish 的轨道（= Gate 造出来的那条）。 */
function trackOf(devices: MediaDevicesStub, index = 0): FakeTrack {
  const track = devices.streams[index]?.getVideoTracks()[0]
  if (!track) throw new Error(`假浏览器没有产出第 ${index} 条轨道`)
  return track
}

describe('课堂会话页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    joinClassroomMock.mockReset().mockResolvedValue(makeJoinResponse())
    leaveSessionMock.mockReset().mockResolvedValue(undefined)
    getClassroomMock.mockReset().mockResolvedValue(OPEN)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('§56：页面里没有任何 <video>（不显示自己的屏幕预览）', async () => {
    installFakePublisherRoom()
    const { wrapper } = await mountSession()

    expect(wrapper.find('video').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('<video')
    // 状态指示本身要在：一句话回答"我到底有没有被看见"。
    expect(wrapper.find('[data-testid="screen-sharing-status"]').text()).toBe('🖥 正在共享整个屏幕')
  })

  it('§56：摄像头 / 麦克风是不可点的说明文字，不是假开关（Phase 9/10）', async () => {
    installFakePublisherRoom()
    const { wrapper } = await mountSession()

    const camera = wrapper.find('[data-testid="camera-row"]')
    const microphone = wrapper.find('[data-testid="microphone-row"]')
    expect(camera.text()).toContain('未启用')
    expect(camera.text()).toContain('Phase 9')
    expect(microphone.text()).toContain('Phase 10')
    // 一个点不动的假开关比没有开关更糟：学生会以为自己打开失败了。
    expect(camera.find('button').exists()).toBe(false)
    expect(camera.find('input').exists()).toBe(false)
    expect(microphone.find('button').exists()).toBe(false)
    expect(microphone.find('input').exists()).toBe(false)
  })

  it('§56：状态面板显示课堂名、当前状态与网络质量', async () => {
    installFakePublisherRoom()
    const { wrapper } = await mountSession()

    expect(wrapper.find('[data-testid="session-classroom-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="session-phase"]').text()).toContain('已进入课堂')
    expect(wrapper.find('[data-testid="connection-quality"]').text()).toBe('良好')
  })

  it('网络质量按 LiveKit 的 ConnectionQuality 映射成中文 + 颜色（§56）', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper } = await mountSession()

    harness.current().emitQuality('excellent')
    await flushPromises()
    expect(wrapper.find('[data-testid="connection-quality"]').text()).toBe('极佳')

    harness.current().emitQuality('lost')
    await flushPromises()
    expect(wrapper.find('[data-testid="connection-quality"]').text()).toBe('已断开')
  })

  it('§20：publish 的是 Gate 造出来的那条轨道，全程只请求过一次屏幕', async () => {
    const harness = installFakePublisherRoom()
    const { devices } = await mountSession()

    expect(devices.calls).toHaveLength(1)
    expect(harness.current().publishedTracks[0]).toBe(trackOf(devices))
  })

  it('§22：轨道 ended → 提示「已停止屏幕共享」+ 恢复入口，不假装还在共享', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper, devices } = await mountSession()

    // 浏览器结束轨道（学生点了"停止共享"）：Gate 的 ended 监听 → 会话 store 的 SCREEN_LOST。
    trackOf(devices).emitEnded()
    await flushPromises()

    const lost = wrapper.find('[data-testid="screen-lost"]')
    expect(lost.exists()).toBe(true)
    expect(lost.text()).toContain('已停止屏幕共享')
    expect(lost.text()).toContain('当前课堂要求持续共享整个屏幕')
    expect(wrapper.find('[data-testid="reshare-screen"]').text()).toBe('重新共享整个屏幕')
    expect(wrapper.find('[data-testid="session-phase"]').text()).toContain('⚠ 已停止屏幕共享')
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(false)
    // 已死的轨道不会被重新发布（再发一次只会得到永远黑屏的"在线"学生）。
    expect(harness.current().publishedTracks).toHaveLength(1)
  })

  it('§22：重新共享 → 重新过 Gate → publish 新轨道，且不重新 join', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper, devices } = await mountSession()

    trackOf(devices).emitEnded()
    await flushPromises()
    expect(wrapper.find('[data-testid="screen-lost"]').exists()).toBe(true)

    await wrapper.find('[data-testid="reshare-screen"]').trigger('click')
    await flushPromises()

    // 第二次 Gate：假浏览器第二次被请求整屏。
    expect(devices.calls).toHaveLength(2)
    expect(harness.current().publishedTracks).toEqual([trackOf(devices, 0), trackOf(devices, 1)])
    // 会话还在，重新 join 只会让老师那端的学生卡片无谓地闪断一次。
    expect(joinClassroomMock).not.toHaveBeenCalled()
    expect(harness.current().connectCalls).toBe(1)
    expect(wrapper.find('[data-testid="screen-lost"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)
  })

  it('§16/§22：重新共享时选了窗口 → 依旧被 Gate 拒绝，不发布任何轨道', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper, devices } = await mountSession()

    trackOf(devices).emitEnded()
    await flushPromises()

    // 学生在第二次选择器里选了"某个窗口"：Gate 必须拒绝（不存在"上次通过就一直放行"）。
    devices.surface = 'window'
    await wrapper.find('[data-testid="reshare-screen"]').trigger('click')
    await flushPromises()

    expect(harness.current().publishedTracks).toHaveLength(1)
    const error = wrapper.find('[data-testid="screen-gate-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain('应用窗口')
    expect(wrapper.find('[data-testid="screen-lost"]').exists()).toBe(true)
  })

  it('媒体连接失败：给出中文提示与重试入口，不白屏', async () => {
    const harness = installFakePublisherRoom({ connectError: new Error('signal failed') })
    const { wrapper, store } = await mountSession()

    expect(store.phase).toBe('media-error')
    const failure = wrapper.find('[data-testid="media-failure"]')
    expect(failure.exists()).toBe(true)
    expect(failure.text()).toContain('无法连接课堂的媒体服务器')

    // 重试：重新 join（拿新 token）→ 再连一次。
    harness.flags.connectError = null
    await wrapper.find('[data-testid="retry-media"]').trigger('click')
    await flushPromises()

    expect(joinClassroomMock).toHaveBeenCalledTimes(1)
    expect(store.phase).toBe('online')
    expect(wrapper.find('[data-testid="media-failure"]').exists()).toBe(false)
  })

  it('离开课堂：上报 leave + 断开 + 停止捕获 + 回课堂列表', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper, router, store, devices } = await mountSession()

    await wrapper.find('[data-testid="leave-classroom"]').trigger('click')
    await flushPromises()

    expect(leaveSessionMock).toHaveBeenCalledWith('session-1')
    expect(harness.current().disconnectCalls).toBe(1)
    expect(trackOf(devices).readyState).toBe('ended')
    expect(store.phase).toBe('left')
    expect(router.currentRoute.value.name).toBe('student-classrooms')
  })

  it('§49：轮询发现课堂被关闭 → 提示 + 断开媒体 + 停止捕获 + 退回列表', async () => {
    vi.useFakeTimers()
    const harness = installFakePublisherRoom()
    const { wrapper, router, devices } = await mountSession()

    getClassroomMock.mockResolvedValue(makeStudentClassroom({ id: 'room-open', status: 'CLOSED' }))
    await vi.advanceTimersByTimeAsync(CLASSROOM_STATUS_POLL_MS)
    await flushPromises()

    const notice = wrapper.find('[data-testid="classroom-closed-notice"]')
    expect(notice.exists()).toBe(true)
    expect(notice.text()).toContain('老师已经关闭本课堂')
    expect(harness.current().disconnectCalls).toBe(1)
    expect(trackOf(devices).readyState).toBe('ended')

    // 提示看得见之后自动回列表。
    await vi.advanceTimersByTimeAsync(4000)
    await flushPromises()
    expect(router.currentRoute.value.name).toBe('student-classrooms')
  })

  it('§44：刷新后（内存里没有凭据）显示「会话信息已丢失」并给回列表入口', async () => {
    installFakePublisherRoom()
    const { wrapper, store } = await mountSession({ prepare: false })

    expect(store.phase).toBe('no-session')
    const missing = wrapper.find('[data-testid="session-missing"]')
    expect(missing.exists()).toBe(true)
    expect(missing.text()).toContain('会话信息已丢失')
    expect(wrapper.find('[data-testid="back-to-classrooms"]').attributes('href')).toBe(
      '/student/classrooms',
    )
    // 绝不显示一个连不上的"连接中…"。
    expect(wrapper.find('[data-testid="session-status-panel"]').exists()).toBe(false)
  })

  it('§44：token 永远不会出现在渲染出来的页面里', async () => {
    installFakePublisherRoom()
    const { wrapper } = await mountSession()

    expect(wrapper.html()).not.toContain('fake-token')
    expect(wrapper.html()).not.toContain('wss://')
  })

  it('自动重连：显示"正在自动重连…"，不把课堂伪装成已断开', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper } = await mountSession()

    harness.current().emitReconnecting()
    await flushPromises()

    expect(wrapper.find('[data-testid="session-reconnecting"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="session-phase"]').text()).toContain('已进入课堂')
  })
})
