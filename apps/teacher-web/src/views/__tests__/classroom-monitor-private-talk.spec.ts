import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenClassroom } from '../../__tests__/fixtures.ts'
import {
  installFakeMonitorRoom,
  installFakeVisibility,
  makeMonitorStudent,
  makeNotJoinedStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  installTeacherMediaDevices,
  makeTeacherMediaDevicesStub,
  micTrackOf,
  type TeacherMediaDevicesStub,
} from '../../__tests__/media-fixtures.ts'
import { useMonitorStore } from '../../stores/monitor.ts'
import { routes } from '../../router'
import ClassroomMonitorView from '../ClassroomMonitorView.vue'

/**
 * 监督墙的私密语音界面测试（§27 / §31 / §32 / §76）。
 *
 * 界面上要证明四件事：
 *
 * 1. 「语音沟通」是**真按钮**：点击 → POST；再点 → DELETE；
 * 2. **当前目标高亮**，而且**同一时刻只有一个**——对另一个人点一下就是切换，
 *    旧卡片立刻不再高亮（后端会撤销旧订阅，界面必须跟着走）；
 * 3. `TEACHER_MIC_REQUIRED` 变成"请先开启你的麦克风" + 一键开麦（§31 的可执行提示）；
 * 4. 听学生的麦克风发生在 **Focus**，面板上写明"正在听谁的麦克风"，
 *    而且任何时刻最多一路音频订阅（§32）。
 */

const {
  getMonitorMock,
  requestMediaTokenMock,
  getClassroomMock,
  startPrivateTalkMock,
  stopPrivateTalkMock,
  getPrivateTalkMock,
} = vi.hoisted(() => ({
  getMonitorMock: vi.fn(),
  requestMediaTokenMock: vi.fn(),
  getClassroomMock: vi.fn(),
  startPrivateTalkMock: vi.fn(),
  stopPrivateTalkMock: vi.fn(),
  getPrivateTalkMock: vi.fn(),
}))

vi.mock('../../lib/teacher-monitor-api.ts', () => ({
  getMonitor: getMonitorMock,
  requestMediaToken: requestMediaTokenMock,
}))

vi.mock('../../lib/private-talk-api.ts', () => ({
  startPrivateTalk: startPrivateTalkMock,
  stopPrivateTalk: stopPrivateTalkMock,
  getPrivateTalk: getPrivateTalkMock,
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
const MEDIA_TOKEN = { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'teacher-token' }

/** 两个都开着麦克风、都在课堂里的学生（切换目标用）。 */
function twoStudents() {
  return [
    makeMonitorStudent({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
      microphone: { active: true },
    }),
    makeMonitorStudent({
      studentId: 's2',
      displayName: '李四',
      sessionId: 'session-2',
      microphone: { active: true },
    }),
  ]
}

let pinia: Pinia
const mounted: VueWrapper[] = []

async function mountView() {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/teacher/classrooms/room-1/monitor')
  await router.isReady()
  const wrapper = mount(ClassroomMonitorView, { global: { plugins: [pinia, router] } })
  mounted.push(wrapper)
  await flushPromises()
  return { wrapper }
}

function tileOf(wrapper: VueWrapper, studentId: string) {
  return wrapper.find(`[data-testid="monitor-tile"][data-student-id="${studentId}"]`)
}

/** 点开某个学生的 Focus 面板。 */
async function focusOn(wrapper: VueWrapper, studentId: string): Promise<void> {
  await tileOf(wrapper, studentId).trigger('click')
  await flushPromises()
}

describe('课堂监督墙 — 私密语音与老师麦克风（§31/§32）', () => {
  let devices: TeacherMediaDevicesStub

  beforeEach(() => {
    pinia = createPinia()
    setActivePinia(pinia)
    devices = makeTeacherMediaDevicesStub()
    installTeacherMediaDevices(devices)
    getMonitorMock.mockReset().mockResolvedValue(twoStudents())
    requestMediaTokenMock.mockReset().mockResolvedValue(MEDIA_TOKEN)
    getClassroomMock.mockReset().mockResolvedValue(CLASSROOM)
    startPrivateTalkMock.mockReset()
    stopPrivateTalkMock.mockReset().mockResolvedValue(undefined)
    getPrivateTalkMock.mockReset().mockResolvedValue(null)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    for (const wrapper of mounted.splice(0)) wrapper.unmount()
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('§31：Focus 的「语音沟通」可用；点击 → POST {studentId}，按钮变成"正在与张三语音沟通 · 结束"', async () => {
    installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const { wrapper } = await mountView()
    await focusOn(wrapper, 's1')

    const talk = wrapper.find('[data-testid="focus-talk"]')
    expect(talk.text()).toBe('语音沟通')

    await talk.trigger('click')
    await flushPromises()

    expect(startPrivateTalkMock).toHaveBeenCalledWith('room-1', 's1')
    expect(wrapper.find('[data-testid="focus-talk"]').text()).toContain('正在与张三语音沟通')
    // §31：老师自己的界面必须一直能回答"我在对谁讲话"。
    expect(wrapper.find('[data-testid="monitor-talk-target"]').text()).toContain(
      '正在与张三语音沟通',
    )
    // 目标卡片上有标记，其他人没有。
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-talk-flag"]').exists()).toBe(true)
    expect(tileOf(wrapper, 's2').find('[data-testid="tile-talk-flag"]').exists()).toBe(false)
  })

  it('§31：当前目标再点一次 → DELETE，全部高亮消失', async () => {
    installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const { wrapper } = await mountView()
    await focusOn(wrapper, 's1')
    await wrapper.find('[data-testid="focus-talk"]').trigger('click')
    await flushPromises()

    await wrapper.find('[data-testid="focus-talk"]').trigger('click')
    await flushPromises()

    expect(stopPrivateTalkMock).toHaveBeenCalledWith('room-1')
    expect(wrapper.find('[data-testid="focus-talk"]').text()).toBe('语音沟通')
    expect(wrapper.find('[data-testid="monitor-talk-target"]').exists()).toBe(false)
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-talk-flag"]').exists()).toBe(false)
  })

  it('§31：切换目标 → 旧卡片不再高亮，只剩新目标（同一时刻只有一个）', async () => {
    installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    startPrivateTalkMock
      .mockResolvedValueOnce({ studentId: 's1', displayName: '张三', sessionId: 'session-1' })
      .mockResolvedValueOnce({ studentId: 's2', displayName: '李四', sessionId: 'session-2' })
    const { wrapper } = await mountView()
    await focusOn(wrapper, 's1')
    await wrapper.find('[data-testid="focus-talk"]').trigger('click')
    await flushPromises()

    // 关掉张三的面板，去点李四（老师就是这么切人的）。
    await wrapper.find('[data-testid="focus-close"]').trigger('click')
    await flushPromises()
    await focusOn(wrapper, 's2')
    await wrapper.find('[data-testid="focus-talk"]').trigger('click')
    await flushPromises()

    expect(startPrivateTalkMock).toHaveBeenNthCalledWith(2, 'room-1', 's2')
    // 旧目标立刻不再高亮——后端撤销旧订阅，界面必须跟上。
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-talk-flag"]').exists()).toBe(false)
    expect(tileOf(wrapper, 's2').find('[data-testid="tile-talk-flag"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="monitor-talk-target"]').text()).toContain('李四')
    expect(wrapper.findAll('[data-testid="tile-talk-flag"]')).toHaveLength(1)
  })

  it('§31：未进入课堂的学生 → 按钮禁用 + 当场说明原因（不要点了才报错）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent({ studentId: 's1', displayName: '王五' }),
    ])
    const { wrapper } = await mountView()
    await focusOn(wrapper, 's1')

    const talk = wrapper.find('[data-testid="focus-talk"]')
    expect((talk.element as HTMLButtonElement).disabled).toBe(true)
    expect(wrapper.find('[data-testid="focus-talk-blocked"]').text()).toContain('尚未进入课堂')

    // 而且真的不会发出请求（面板只是"没得点"，后端不会被骚扰）。
    expect(startPrivateTalkMock).not.toHaveBeenCalled()
  })

  it('§31：TEACHER_MIC_REQUIRED → 显示"请先开启你的麦克风"，并给一键开麦', async () => {
    installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    const { ApiError } = await import('@classwatch/api-client')
    startPrivateTalkMock.mockRejectedValue(
      new ApiError({ code: 'TEACHER_MIC_REQUIRED', message: 'mic required', status: 409 }),
    )
    const { wrapper } = await mountView()
    await focusOn(wrapper, 's1')

    await wrapper.find('[data-testid="focus-talk"]').trigger('click')
    await flushPromises()

    const required = wrapper.find('[data-testid="focus-talk-mic-required"]')
    expect(required.exists()).toBe(true)
    expect(required.text()).toContain('请先开启你的麦克风')

    // 一键开麦真的请求设备并发布（§27）。
    await wrapper.find('[data-testid="focus-talk-enable-mic"]').trigger('click')
    await flushPromises()

    expect(devices.micCalls).toHaveLength(1)
    expect(devices.micCalls[0]?.constraints).toEqual({ audio: true })
    expect(useMonitorStore(pinia).teacherMicState).toBe('on')
  })

  it('§31：其他失败（目标不在课堂）只给一句可读的话', async () => {
    installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    const { ApiError } = await import('@classwatch/api-client')
    startPrivateTalkMock.mockRejectedValue(
      new ApiError({ code: 'PRIVATE_TALK_UNAVAILABLE', message: 'unavailable', status: 409 }),
    )
    const { wrapper } = await mountView()
    await focusOn(wrapper, 's1')

    await wrapper.find('[data-testid="focus-talk"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="focus-talk-error"]').text()).toContain('不在课堂中')
    expect(wrapper.find('[data-testid="focus-talk-mic-required"]').exists()).toBe(false)
  })

  it('§27：监督墙上可以直接开关老师自己的麦克风（关 = unpublish + stop）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="toggle-teacher-mic"]').trigger('click')
    await flushPromises()

    expect(devices.micCalls).toHaveLength(1)
    expect(harness.current().publishedMicrophoneTracks).toHaveLength(1)
    expect(wrapper.find('[data-testid="teacher-mic-status"]').text()).toBe('已开启')

    await wrapper.find('[data-testid="toggle-teacher-mic"]').trigger('click')
    await flushPromises()

    expect(harness.current().unpublishMicrophoneCalls).toBe(1)
    expect(micTrackOf(devices).readyState).toBe('ended')
    expect(wrapper.find('[data-testid="teacher-mic-status"]').text()).toBe('未开启')
  })

  it('§32：Focus 中订阅该学生的麦克风，并在面板上写明"正在听谁的麦克风"', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    const { wrapper } = await mountView()

    await focusOn(wrapper, 's1')

    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-1'])
    expect(wrapper.find('[data-testid="focus-listening"]').text()).toBe('正在听张三的麦克风')
    // "正在听"背后必须真的有一条音频挂在元素上，否则这句话是空的。
    expect(wrapper.find('[data-testid="focus-mic-audio"]').exists()).toBe(true)
  })

  it('§32：学生没开麦时说清"未开启"；退出 Focus 立刻取消订阅', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', displayName: '张三', sessionId: 'session-1' }),
      makeMonitorStudent({ studentId: 's2', displayName: '李四', sessionId: 'session-2' }),
    ])
    const { wrapper } = await mountView()

    await focusOn(wrapper, 's1')
    expect(harness.current().subscribeMicrophoneCalls).toEqual([])
    expect(wrapper.find('[data-testid="focus-listening"]').text()).toBe('该学生未开启麦克风')

    // 换成开着麦的学生：订上了。
    await wrapper.find('[data-testid="focus-close"]').trigger('click')
    await flushPromises()
    getMonitorMock.mockResolvedValue(twoStudents())
    await useMonitorStore(pinia).refresh()
    await flushPromises()
    await focusOn(wrapper, 's2')
    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-2'])

    await wrapper.find('[data-testid="focus-close"]').trigger('click')
    await flushPromises()

    // §32：退出 Focus 就不再听——而且任何时刻最多一路音频。
    expect(harness.current().unsubscribeMicrophoneCalls).toEqual(['session-2'])
    expect(Object.keys(useMonitorStore(pinia).microphoneSubscriptions)).toEqual([])
  })

  it('§32：切换 Focus 时最多只有一路音频订阅（旧的必须退掉）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    const { wrapper } = await mountView()

    await focusOn(wrapper, 's1')
    await wrapper.find('[data-testid="focus-close"]').trigger('click')
    await flushPromises()
    await focusOn(wrapper, 's2')

    expect(harness.current().unsubscribeMicrophoneCalls).toEqual(['session-1'])
    expect(Object.keys(useMonitorStore(pinia).microphoneSubscriptions)).toEqual(['session-2'])
  })
})
