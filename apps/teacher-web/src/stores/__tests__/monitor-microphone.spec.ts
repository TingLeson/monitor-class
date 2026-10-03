import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  installFakeMonitorRoom,
  installFakeVisibility,
  makeLeftStudent,
  makeMonitorStudent,
  makeNotJoinedStudent,
} from '../../__tests__/monitor-fixtures.ts'
import { makeMicChangedEvent, makeStudentOfflineEvent } from '../../__tests__/realtime-fixtures.ts'
import { useMonitorStore } from '../monitor.ts'

/**
 * 老师端"听学生麦克风"的订阅测试（§32 / §76）。
 *
 * Phase 10 在这一条线上的全部风险都集中在两句话上：
 *
 * 1. **只订 Focus 的那一个**（§32）。网格里同时开多路音频会让老师分辨不出是谁在说话，
 *    这条能力本身就此失去意义——所以本文件里最重要的断言是"任何时刻最多一路音频订阅"；
 * 2. **该退就必须退**：退出 Focus、学生关麦、学生离线、离开课堂，
 *    四种情况都必须立刻取消订阅（否则老师的耳机里会一直有个不知道是谁的声音）。
 *
 * 另外 `MIC_CHANGED` 只许改 `microphone` 一个字段（§80 的"不造数据"），
 * 并且必须触发一轮订阅协调——Focus 里的学生一开麦就要能马上听到。
 */

const { getMonitorMock, requestMediaTokenMock } = vi.hoisted(() => ({
  getMonitorMock: vi.fn(),
  requestMediaTokenMock: vi.fn(),
}))

vi.mock('../../lib/teacher-monitor-api.ts', () => ({
  getMonitor: getMonitorMock,
  requestMediaToken: requestMediaTokenMock,
}))

vi.mock('../../lib/private-talk-api.ts', () => ({
  startPrivateTalk: vi.fn(),
  stopPrivateTalk: vi.fn(),
  getPrivateTalk: vi.fn().mockResolvedValue(null),
}))

const MEDIA_TOKEN = {
  livekitUrl: 'wss://classwatch-test.livekit.cloud',
  token: 'teacher-token',
}

let pinia: Pinia

/** 麦克风开着的、已经进入课堂的学生。 */
function micStudent(overrides: Parameters<typeof makeMonitorStudent>[0] = {}) {
  return makeMonitorStudent({ microphone: { active: true }, ...overrides })
}

describe('监督 store — 听学生的麦克风（§32）', () => {
  beforeEach(() => {
    pinia = createPinia()
    setActivePinia(pinia)
    getMonitorMock.mockReset().mockResolvedValue([micStudent()])
    requestMediaTokenMock.mockReset().mockResolvedValue(MEDIA_TOKEN)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('§32：只订 **Focus** 那一个学生的麦克风（可见的网格卡片不订音频）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      micStudent({ studentId: 's1', sessionId: 'session-1', displayName: '张三' }),
      micStudent({ studentId: 's2', sessionId: 'session-2', displayName: '李四' }),
    ])
    const store = useMonitorStore(pinia)
    // 两张卡片都在视口里（网格可见），但只有 Focus 的那个该被听到。
    store.setStudentVisible('s1', true)
    store.setStudentVisible('s2', true)
    await store.load('room-1')

    expect(harness.current().subscribeMicrophoneCalls).toEqual([])

    store.setFocusedStudent('s1')
    await flushPromises()

    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-1'])
    expect(store.isListeningToMicrophone(store.students[0]!)).toBe(true)
    expect(store.microphoneMediaStateOf(store.students[0]!)).toBe('subscribed')
  })

  it('§32：任何时刻最多只有一路音频订阅（切换 Focus 时旧的必须退掉）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1', 'session-2'] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      micStudent({ studentId: 's1', sessionId: 'session-1' }),
      micStudent({ studentId: 's2', sessionId: 'session-2' }),
    ])
    const store = useMonitorStore(pinia)
    store.setStudentVisible('s1', true)
    store.setStudentVisible('s2', true)
    await store.load('room-1')

    store.setFocusedStudent('s1')
    await flushPromises()
    store.setFocusedStudent('s2')
    await flushPromises()

    // 旧的退了、新的订上了。
    expect(harness.current().unsubscribeMicrophoneCalls).toEqual(['session-1'])
    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-1', 'session-2'])
    expect(Object.keys(store.microphoneSubscriptions)).toEqual(['session-2'])
  })

  it('§32：退出 Focus（或页面切走）立刻取消订阅——老师不该继续听一个没人看的画面', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()
    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-1'])

    store.setFocusedStudent(null)
    await flushPromises()

    expect(harness.current().unsubscribeMicrophoneCalls).toEqual(['session-1'])
    expect(store.microphoneSubscriptions).toEqual({})
  })

  it('§32：学生麦克风没开就不订（DTO 说了算，不看媒体层有没有轨道）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([makeMonitorStudent({ microphone: { active: false } })])
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()

    expect(harness.current().subscribeMicrophoneCalls).toEqual([])
    expect(store.isListeningToMicrophone(store.students[0]!)).toBe(false)
  })

  it('§32：Focus 里的学生**开麦**（MIC_CHANGED active=true）→ 立刻订上', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([makeMonitorStudent({ microphone: { active: false } })])
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()
    expect(harness.current().subscribeMicrophoneCalls).toEqual([])

    store.applyRealtimeEvent(makeMicChangedEvent({ active: true }))
    await flushPromises()

    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-1'])
  })

  it('§32：Focus 里的学生**关麦**（MIC_CHANGED active=false）→ 立刻退掉那一路', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()
    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-1'])

    store.applyRealtimeEvent(makeMicChangedEvent({ active: false }))
    await flushPromises()

    expect(harness.current().unsubscribeMicrophoneCalls).toEqual(['session-1'])
    expect(store.microphoneMediaStateOf(store.students[0]!)).toBe('none')
  })

  it('§80/§51：MIC_CHANGED 只改 microphone 一个字段，其它字段一个都不许动', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    const before = store.students[0]!

    store.applyRealtimeEvent(makeMicChangedEvent({ active: true }))
    const after = store.students[0]!

    expect(after.microphone).toEqual({ active: true })
    // 事件证明不了这些字段：顺手"补一个看起来合理"的值就是造数据。
    expect(after.screen).toEqual(before.screen)
    expect(after.camera).toEqual(before.camera)
    expect(after.sessionStatus).toBe(before.sessionStatus)
    expect(after.connection).toBe(before.connection)
    expect(after.joinedAt).toBe(before.joinedAt)
    expect(after.lastEventAt).toBe(before.lastEventAt)
  })

  it('§32：学生离线 → 麦克风订阅与"正在听"的指示一起消失', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()

    store.applyRealtimeEvent(makeStudentOfflineEvent({ reason: 'LEFT' }))
    await flushPromises()

    expect(harness.current().unsubscribeMicrophoneCalls).toEqual(['session-1'])
    expect(store.isListeningToMicrophone(store.students[0]!)).toBe(false)
    expect(store.students[0]?.microphone).toEqual({ active: false })
  })

  it('§32：媒体层说参与者已经不在房间里 → 立刻丢掉本地订阅（不等下一次快照）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1'] })
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()
    expect(store.isListeningToMicrophone(store.students[0]!)).toBe(true)

    // 媒体里人没了，而 monitor DTO 还停留在"他在线"（最多 60 秒才刷一次）。
    harness.current().participants = []
    harness.current().emitParticipantsChanged()
    await flushPromises()

    expect(store.isListeningToMicrophone(store.students[0]!)).toBe(false)
  })

  it('§32：未进入课堂的学生即使被 Focus 也不会订音频（没有 identity 就没有轨道）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent({ studentId: 's1', microphone: { active: true } }),
    ])
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    store.setFocusedStudent('s1')
    await flushPromises()

    expect(harness.current().subscribeMicrophoneCalls).toEqual([])
  })

  it('§32：已经离开的学生不会被订（会话不在线，`shouldSubscribeMicrophone` 直接拒绝）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeLeftStudent({ studentId: 's1', microphone: { active: true } }),
    ])
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    store.setFocusedStudent('s1')
    await flushPromises()

    expect(harness.current().subscribeMicrophoneCalls).toEqual([])
  })

  it('§32：订阅失败（媒体侧）只标记为 failed，屏幕与摄像头的订阅完全不受影响', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    harness.flags.subscribeMicrophoneError = new Error('subscribe failed')
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()

    expect(store.microphoneMediaStateOf(store.students[0]!)).toBe('failed')
    // 屏幕那条照常订上：三条轨道互不影响。
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
    expect(store.subscriptionOf(store.students[0]!)).not.toBeNull()
  })

  it('§32：没有这条麦克风轨道时返回 null，界面显示"正在连接…"而不是报错', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    // 房间是在 load 里建的：这里才轮到"业务说他开着麦，媒体里还没有轨道"。
    harness.current().microphoneAvailable = false
    store.setFocusedStudent('student-1')
    await flushPromises()

    expect(store.microphoneMediaStateOf(store.students[0]!)).toBe('none')
    expect(store.isListeningToMicrophone(store.students[0]!)).toBe(false)
  })

  it('§52：同一个 participant 不重复订阅麦克风（十秒一轮的刷新不成订阅风暴）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    store.setStudentVisible('student-1', true)
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()

    await store.reconcileSubscriptions()
    await store.refresh()
    await flushPromises()

    expect(harness.current().subscribeMicrophoneCalls).toEqual(['session-1'])
  })
})
