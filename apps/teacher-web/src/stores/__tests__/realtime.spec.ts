import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeClassroom, makeOpenClassroom } from '../../__tests__/fixtures.ts'
import {
  installFakeRealtimeSocket,
  makeMicChangedEvent,
  makePrivateTalkEndedEvent,
  makeRoomClosedEvent,
  makeScreenLostEvent,
  makeStudentOnlineEvent,
  makeTeacherPrivateTalkStartedEvent,
  type RealtimeHarness,
} from '../../__tests__/realtime-fixtures.ts'
import { makeMonitorStudent } from '../../__tests__/monitor-fixtures.ts'
import { TEACHER_REALTIME_PATH } from '../../lib/realtime-channel.ts'
import { useClassroomsStore } from '../classrooms.ts'
import { useMonitorStore } from '../monitor.ts'
import { useRealtimeStore } from '../realtime.ts'

/**
 * 老师端实时通道的路由测试（§47 / §49 / §51）。
 *
 * 这一份盯的是"事件改了谁"以及"事件没改谁"：
 *
 * 1. `ROOM_CLOSED` → 监督墙 + 课堂详情/列表都要跟着变（§49：老师从另一个标签页关课）；
 * 2. 学生类事件只给监督墙（课堂列表不关心谁在线）；
 * 3. 连接地址是同源 `/ws/teacher`，且**不含凭据**（§44）；
 * 4. 重连成功后自动补一次快照（断线期间的事件是永久丢失的）。
 */

const { getMonitorMock, requestMediaTokenMock } = vi.hoisted(() => ({
  getMonitorMock: vi.fn(),
  requestMediaTokenMock: vi.fn(),
}))

vi.mock('../../lib/teacher-monitor-api.ts', () => ({
  getMonitor: getMonitorMock,
  requestMediaToken: requestMediaTokenMock,
}))

/**
 * 私密语音接口（§31）。`load()` 会读一次"当前目标"来恢复界面记忆，
 * 不替身的话每个用例都会真的去 fetch 一次（既慢又会打出连接失败的噪音）。
 */
vi.mock('../../lib/private-talk-api.ts', () => ({
  startPrivateTalk: vi.fn(),
  stopPrivateTalk: vi.fn(),
  getPrivateTalk: vi.fn().mockResolvedValue(null),
}))

const { getClassroomMock, listClassroomsMock } = vi.hoisted(() => ({
  getClassroomMock: vi.fn(),
  listClassroomsMock: vi.fn(),
}))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: getClassroomMock,
  createClassroom: vi.fn(),
  updateClassroom: vi.fn(),
  listClassroomStudents: vi.fn().mockResolvedValue({ students: [] }),
  addClassroomStudents: vi.fn(),
  removeClassroomStudent: vi.fn(),
  openClassroom: vi.fn(),
  closeClassroom: vi.fn(),
}))

let harness: RealtimeHarness

describe('老师端实时通道', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    harness = installFakeRealtimeSocket()
    getMonitorMock.mockReset().mockResolvedValue([])
    requestMediaTokenMock
      .mockReset()
      .mockResolvedValue({ livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'tok' })
    listClassroomsMock.mockReset().mockResolvedValue([makeOpenClassroom({ id: 'room-1' })])
    getClassroomMock.mockReset().mockResolvedValue(makeOpenClassroom({ id: 'room-1' }))
    vi.spyOn(console, 'warn').mockImplementation(() => undefined)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('§44：连接同源 /ws/teacher，地址里没有任何凭据', () => {
    const realtime = useRealtimeStore()

    realtime.start()

    const socket = harness.current()
    expect(new URL(socket.url).pathname).toBe(TEACHER_REALTIME_PATH)
    expect(socket.url).not.toMatch(/token|cookie|auth=/i)
    expect(realtime.state).toBe('connecting')
  })

  it('start 幂等：一个入口只有一条连接（否则事件会被处理两遍）', () => {
    const realtime = useRealtimeStore()

    realtime.start()
    realtime.start()

    expect(harness.sockets).toHaveLength(1)
  })

  it('§49：ROOM_CLOSED 同时改监督墙与课堂详情（老师从另一个标签页关课）', async () => {
    const classrooms = useClassroomsStore()
    const monitor = useMonitorStore()
    await classrooms.fetchList()
    await classrooms.fetchDetail('room-1')
    await monitor.load('room-1')

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()
    harness.emit(makeRoomClosedEvent({ classroomId: 'room-1' }))

    // 关键：事件是**同步**生效的。这一刻 HTTP 快照还没回来，
    // 所以下面这些结论只可能来自实时事件（§47）。
    expect(monitor.classroomClosed).toBe(true)
    expect(classrooms.current?.status).toBe('CLOSED')
    expect(classrooms.current?.currentRun).toBeNull()
    expect(classrooms.items[0]?.status).toBe('CLOSED')
    // 详情页要有一句说明：徽章静悄悄地变灰，老师会以为是自己点错了。
    expect(classrooms.realtimeNotice).toContain('其他页面')

    // 之后快照收敛：真实后端在课堂关闭后返回的自然也是 CLOSED。
    listClassroomsMock.mockResolvedValue([makeClassroom({ id: 'room-1', status: 'CLOSED' })])
    getClassroomMock.mockResolvedValue(makeClassroom({ id: 'room-1', status: 'CLOSED' }))
    await flushPromises()
    expect(classrooms.current?.status).toBe('CLOSED')
    expect(classrooms.items[0]?.status).toBe('CLOSED')
  })

  it('§47：事件之前发出、之后才回来的过期快照不会把结论改回去', async () => {
    const classrooms = useClassroomsStore()
    await classrooms.fetchList()

    /**
     * 真实场景：WS 重连后页面立刻补一次快照，服务端随即把排队的事件推过来。
     * 第一份快照取的是"关课之前"的时刻（OPEN），直接覆盖就会让详情页闪回"已开启"。
     */
    getClassroomMock.mockResolvedValueOnce(makeOpenClassroom({ id: 'room-1' }))
    getClassroomMock.mockResolvedValue(makeClassroom({ id: 'room-1', status: 'CLOSED' }))

    const pending = classrooms.fetchDetail('room-1')
    classrooms.applyRealtimeEvent(makeRoomClosedEvent({ classroomId: 'room-1' }))
    await pending
    await flushPromises()

    expect(classrooms.current?.status).toBe('CLOSED')
  })

  it('学生类事件只给监督墙（课堂列表不关心谁在线）', async () => {
    const classrooms = useClassroomsStore()
    const monitor = useMonitorStore()
    await classrooms.fetchList()
    await monitor.load('room-1')
    getMonitorMock.mockResolvedValue([
      {
        studentId: 'student-1',
        displayName: '张三',
        sessionId: null,
        sessionStatus: null,
        screen: { active: false },
        camera: { active: false },
        microphone: { active: false },
        connection: 'UNKNOWN',
        joinedAt: null,
        lastEventAt: null,
      },
    ])
    await monitor.refresh()

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()
    harness.emit(makeStudentOnlineEvent({ sessionId: 'session-1' }))
    harness.emit(makeScreenLostEvent({ sessionId: 'session-1' }))
    await flushPromises()

    expect(monitor.students[0]?.sessionStatus).toBe('SCREEN_LOST')
    // 课堂列表的状态没有被这些学生事件带偏。
    expect(classrooms.items[0]?.status).toBe('OPEN')
  })

  it('§31/§32：MIC_CHANGED 与 PRIVATE_TALK_* 都路由到监督 store', async () => {
    const monitor = useMonitorStore()
    await monitor.load('room-1')
    getMonitorMock.mockResolvedValue([makeMonitorStudent({ microphone: { active: false } })])
    await monitor.refresh()

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()

    harness.emit(makeMicChangedEvent({ active: true }))
    expect(monitor.students[0]?.microphone).toEqual({ active: true })

    // 目标可能是在另一个标签页里发起的：这条事件必须真的走进 store。
    harness.emit(
      makeTeacherPrivateTalkStartedEvent({
        studentId: 'student-1',
        sessionId: 'session-1',
        displayName: '张三',
      }),
    )
    expect(monitor.talkTarget?.studentId).toBe('student-1')

    // 沟通结束时后端会广播 ENDED：界面必须能回到"无目标"。
    harness.emit(makePrivateTalkEndedEvent({ sessionId: 'session-1' }))
    expect(monitor.talkTarget).toBeNull()
  })

  it('§47：重连成功后自动补一次快照（断线期间的事件补不回来）', async () => {
    const monitor = useMonitorStore()
    await monitor.load('room-1')
    const callsAfterLoad = getMonitorMock.mock.calls.length

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()
    await flushPromises()
    harness.close()
    // 退避重连（假时钟）→ 新连接 → 再次 open。
    vi.useFakeTimers()
    await vi.advanceTimersByTimeAsync(2_000)
    harness.current().open()
    await flushPromises()

    expect(getMonitorMock.mock.calls.length).toBeGreaterThan(callsAfterLoad)
    expect(realtime.wasConnected).toBe(true)
  })

  it('stop：关闭底层连接并复位状态（登出/卸载不留悬挂通道）', () => {
    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()

    realtime.stop()

    expect(harness.current().closeCalls).toBe(1)
    expect(realtime.state).toBe('closed')
    expect(realtime.started).toBe(false)
    expect(realtime.wasConnected).toBe(false)
  })
})
