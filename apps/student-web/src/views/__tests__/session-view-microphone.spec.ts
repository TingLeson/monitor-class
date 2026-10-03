import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { installFakePublisherRoom, makeJoinResponse } from '../../__tests__/media-fixtures'
import { makeOpenStudentClassroom } from '../../__tests__/fixtures'
import {
  installMediaDevices,
  makeMediaDevicesStub,
  type MediaDevicesStub,
} from '../../__tests__/screen-fixtures'
import {
  makePrivateTalkEndedEvent,
  makePrivateTalkRequestEvent,
  makeStudentPrivateTalkStartedEvent,
} from '../../__tests__/realtime-fixtures.ts'
import { requestEntireScreen, type ScreenCapture } from '../../lib/screen-capture'
import { routes } from '../../router'
import SessionView from '../SessionView.vue'

/**
 * 会话页的麦克风与私密语音界面测试（§25 / §26 / §31）。
 *
 * 界面这一层要证明的只有四件事：
 *
 * 1. `🎤 开启麦克风` 是真的设备开关，**点击才** `getUserMedia`；
 * 2. 老师的请求只以"提示 + 两个按钮"出现，而且点「开启麦克风」之前一次设备请求都没有
 *    （§25：老师不能绕过浏览器权限）；
 * 3. 「暂不开启」之后界面仍然说明"老师能单向讲话"，持续指示也在；
 * 4. 页面里**没有**其他学生的任何信息，也没有屏幕预览（§26/§56）。
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

async function gateCapture(): Promise<{ devices: MediaDevicesStub; capture: ScreenCapture }> {
  const devices = makeMediaDevicesStub()
  installMediaDevices(devices)
  const capture = await requestEntireScreen()
  return { devices, capture }
}

/** 把会话页挂起来（与 session-view.spec.ts 同一套装配）。 */
async function mountSession() {
  const { useMediaSessionStore } = await import('../../stores/media-session.ts')
  const store = useMediaSessionStore()
  const { devices, capture } = await gateCapture()
  store.prepare({
    sessionId: 'session-1',
    classroomId: 'room-open',
    credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'fake-token' },
    capture,
  })

  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/student/session/session-1')
  await router.isReady()
  const wrapper = mount(SessionView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, store, devices }
}

describe('课堂会话页 — 麦克风与私密语音（§25/§31）', () => {
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

  it('§25：挂载时不请求麦克风，点击之后才有第一次 `{audio:true}`', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper, devices } = await mountSession()

    expect(devices.micCalls).toHaveLength(0)
    expect(wrapper.find('[data-testid="microphone-status"]').text()).toBe('未开启')

    await wrapper.find('[data-testid="toggle-microphone"]').trigger('click')
    await flushPromises()

    expect(devices.micCalls).toHaveLength(1)
    expect(devices.micCalls[0]?.constraints).toEqual({ audio: true })
    expect(harness.current().publishedMicrophoneTracks).toHaveLength(1)
    expect(wrapper.find('[data-testid="microphone-status"]').text()).toBe('已开启')
    expect(wrapper.find('[data-testid="toggle-microphone"]').text()).toBe('关闭麦克风')
    // 界面上没有麦克风预览（§56 只要求状态指示）。
    expect(wrapper.find('[data-testid="microphone-row"]').find('input').exists()).toBe(false)
  })

  it('§25：权限被拒 → 可执行的提示，且课堂与屏幕共享完全不受影响', async () => {
    installFakePublisherRoom()
    const { wrapper, devices, store } = await mountSession()
    const denied = new Error('denied')
    denied.name = 'NotAllowedError'
    devices.micFailWith = denied

    await wrapper.find('[data-testid="toggle-microphone"]').trigger('click')
    await flushPromises()

    const failure = wrapper.find('[data-testid="microphone-failure"]')
    expect(failure.exists()).toBe(true)
    expect(failure.text()).toContain('没有授予麦克风权限')
    expect(failure.text()).toContain('仍然能听到老师讲话')
    // 课堂照旧：屏幕还在共享、会话状态没变、没有媒体失败告警（§21）。
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="session-phase"]').text()).toContain('已进入课堂')
    expect(wrapper.find('[data-testid="media-failure"]').exists()).toBe(false)
    expect(store.phase).toBe('online')
  })

  it('§25：老师的请求显示姓名与两个按钮，**不点就不开麦**', async () => {
    installFakePublisherRoom()
    const { wrapper, devices, store } = await mountSession()

    store.applyRealtimeEvent(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))
    await flushPromises()

    const prompt = wrapper.find('[data-testid="private-talk-request"]')
    expect(prompt.exists()).toBe(true)
    // §25 的原文。
    expect(wrapper.find('[data-testid="private-talk-request-title"]').text()).toBe(
      '王老师希望与你进行语音沟通。',
    )
    expect(wrapper.find('[data-testid="private-talk-accept"]').text()).toBe('开启麦克风')
    expect(wrapper.find('[data-testid="private-talk-decline"]').text()).toBe('暂不开启')
    // 关键：收到事件**不等于**开麦——老师不能绕过浏览器权限（§25）。
    expect(devices.micCalls).toHaveLength(0)
    expect(store.micState).toBe('off')
  })

  it('§25：点「开启麦克风」才 getUserMedia；成功后提示变成持续指示', async () => {
    installFakePublisherRoom()
    const { wrapper, devices, store } = await mountSession()
    store.applyRealtimeEvent(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))
    await flushPromises()

    await wrapper.find('[data-testid="private-talk-accept"]').trigger('click')
    await flushPromises()

    expect(devices.micCalls).toHaveLength(1)
    expect(wrapper.find('[data-testid="private-talk-request"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="private-talk-active-text"]').text()).toBe(
      '正在与王老师语音沟通',
    )
  })

  it('§25：点「暂不开启」→ 提示关闭，并说明"老师仍能单向讲话"', async () => {
    installFakePublisherRoom()
    const { wrapper, store } = await mountSession()
    store.applyRealtimeEvent(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))
    await flushPromises()

    await wrapper.find('[data-testid="private-talk-decline"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="private-talk-request"]').exists()).toBe(false)
    // 沟通没有结束：老师仍然在讲话，指示与说明都必须在（§25）。
    expect(wrapper.find('[data-testid="private-talk-active"]').exists()).toBe(true)
    const note = wrapper.find('[data-testid="private-talk-one-way-note"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('你仍然能听到老师讲话')
    expect(note.text()).toContain('老师听不到你')
  })

  it('§25：持续指示只能由老师那侧的事件结束（PRIVATE_TALK_ENDED）', async () => {
    installFakePublisherRoom()
    const { wrapper, store } = await mountSession()
    store.applyRealtimeEvent(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))
    await flushPromises()
    expect(wrapper.find('[data-testid="private-talk-active-text"]').text()).toBe(
      '正在与王老师语音沟通',
    )

    store.applyRealtimeEvent(makePrivateTalkEndedEvent({ sessionId: 'session-1' }))
    await flushPromises()

    expect(wrapper.find('[data-testid="private-talk-active"]').exists()).toBe(false)
  })

  it('§31：开着麦克风时可以"静音自己"，界面如实显示已静音且说明老师听不到', async () => {
    installFakePublisherRoom()
    const { wrapper, store } = await mountSession()
    store.applyRealtimeEvent(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))
    await wrapper.find('[data-testid="toggle-microphone"]').trigger('click')
    await flushPromises()

    await wrapper.find('[data-testid="private-talk-self-mute"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="private-talk-self-mute"]').text()).toBe('取消静音')
    expect(wrapper.find('[data-testid="microphone-status"]').text()).toBe('已开启（已静音）')
    expect(wrapper.find('[data-testid="private-talk-one-way-note"]').exists()).toBe(true)
  })

  it('§31：老师语音被浏览器拦住时，界面必须给"点击播放声音"而不是假装在放', async () => {
    const harness = installFakePublisherRoom()
    const { wrapper, store } = await mountSession()
    store.applyRealtimeEvent(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))
    harness.current().emitTeacherAudio('blocked')
    await flushPromises()

    const blocked = wrapper.find('[data-testid="private-talk-audio-blocked"]')
    expect(blocked.exists()).toBe(true)
    expect(blocked.text()).toContain('浏览器')

    await wrapper.find('[data-testid="private-talk-resume-audio"]').trigger('click')
    await flushPromises()

    expect(harness.current().resumeAudioCalls).toBe(1)
    expect(wrapper.find('[data-testid="private-talk-audio-blocked"]').exists()).toBe(false)
  })

  it('§26/§56：页面里没有其他学生的任何信息，也没有屏幕预览', async () => {
    installFakePublisherRoom()
    const { wrapper, store } = await mountSession()
    store.applyRealtimeEvent(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))
    await flushPromises()

    const html = wrapper.html()
    // 没有其他学生的痕迹：没有名字、没有"参与人数"、没有学生列表。
    expect(html).not.toContain('张三')
    expect(html).not.toContain('同学')
    expect(html).not.toContain('参与人数')
    expect(html).not.toContain('在线人数')
    // 没有任何屏幕预览（§56：避免 screen-in-screen）；摄像头自视窗只在开摄像头后出现。
    expect(wrapper.findAll('video')).toHaveLength(0)
    expect(wrapper.findAll('audio')).toHaveLength(0)
  })
})
