import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  installFakeRealtimeSocket,
  makePrivateTalkEndedEvent,
  makePrivateTalkRequestEvent,
  makeRoomClosedEvent,
  makeRoomOpenedEvent,
  makeScreenLostEvent,
  makeScreenRestoredEvent,
  makeStudentPrivateTalkStartedEvent,
  makeTeacherShapedPrivateTalkStartedEvent,
  type RealtimeHarness,
} from '../../__tests__/realtime-fixtures.ts'
import { makeStudentClassroom } from '../../__tests__/fixtures.ts'
import { makeFakeCapture } from '../../__tests__/screen-fixtures.ts'
import { STUDENT_REALTIME_PATH } from '../../lib/realtime-channel.ts'
import { useClassroomsStore } from '../classrooms.ts'
import { useRealtimeStore } from '../realtime.ts'

/**
 * 学生端实时通道的路由测试（§26 / §47 / §48 / §49）。
 *
 * 这一份盯的是"事件到底改了谁"：
 *
 * 1. `ROOM_OPENED` → 列表里的课堂变成可进入（§48：学生 dashboard 自动更新）；
 * 2. `ROOM_CLOSED` → 列表状态 + 在课会话两处都要变（§49）；
 * 3. 屏幕类事件只给在课会话（§22/§46）；
 * 4. 连接地址是同源路径，且**不含凭据**（§44）；
 * 5. 停止后底层连接真的被关掉（HMR / 登出不留悬挂连接）。
 *
 * 报文一律走假 socket 的真实链路（JSON → 守卫 → 路由），不直接调 store 方法——
 * 那样测的就只是"我自己调我自己"。
 */

const { listClassroomsMock, getClassroomMock } = vi.hoisted(() => ({
  listClassroomsMock: vi.fn(),
  getClassroomMock: vi.fn(),
}))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: getClassroomMock,
}))

let harness: RealtimeHarness

describe('学生端实时通道', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    harness = installFakeRealtimeSocket()
    listClassroomsMock.mockResolvedValue([makeStudentClassroom({ id: 'room-1', status: 'CLOSED' })])
    // 兜底轮询会调它；默认返回"课堂还开着"，这样任何"变成 closed"的断言
    // 都只可能来自实时事件，而不是被一次轮询顺手改掉。
    getClassroomMock
      .mockReset()
      .mockResolvedValue(makeStudentClassroom({ id: 'room-1', status: 'OPEN' }))
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('§44：连接同源 /ws/student，地址里没有任何凭据', () => {
    const realtime = useRealtimeStore()

    realtime.start()

    const socket = harness.current()
    expect(new URL(socket.url).pathname).toBe(STUDENT_REALTIME_PATH)
    expect(socket.url).not.toMatch(/token|cookie|auth=/i)
    expect(realtime.state).toBe('connecting')
  })

  it('start 幂等：重复调用不会建第二条连接（否则事件会被处理两遍）', () => {
    const realtime = useRealtimeStore()

    realtime.start()
    realtime.start()

    expect(harness.sockets).toHaveLength(1)
  })

  it('§48：ROOM_OPENED 让列表里的课堂自动变成可进入', async () => {
    const classrooms = useClassroomsStore()
    await classrooms.fetchList()
    expect(classrooms.items[0]?.status).toBe('CLOSED')

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()
    harness.emit(makeRoomOpenedEvent({ classroomId: 'room-1', runId: 'run-9' }))
    await flushPromises()

    expect(realtime.state).toBe('open')
    expect(realtime.lastEventAt).toBe('2026-10-03T10:00:00Z')
    expect(classrooms.items[0]?.status).toBe('OPEN')
    // runId 来自事件，正好是卡片上"本次开始于"要用的 currentRun。
    expect(classrooms.items[0]?.currentRun).toEqual({
      id: 'run-9',
      openedAt: '2026-10-03T10:00:00Z',
    })
  })

  it('§26：不在我名单里的课堂被忽略（绝不凭空造一张卡片）', async () => {
    const classrooms = useClassroomsStore()
    await classrooms.fetchList()

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()
    harness.emit(makeRoomOpenedEvent({ classroomId: 'someone-elses-room' }))

    expect(classrooms.items).toHaveLength(1)
    expect(classrooms.items[0]?.id).toBe('room-1')
  })

  it('§49：ROOM_CLOSED 同时改列表与在课会话（两处不能自相矛盾）', async () => {
    const classrooms = useClassroomsStore()
    await classrooms.fetchList()
    classrooms.current = makeStudentClassroom({ id: 'room-1', status: 'OPEN' })

    const { useMediaSessionStore } = await import('../media-session.ts')
    const session = useMediaSessionStore()
    session.prepare({
      sessionId: 'session-1',
      classroomId: 'room-1',
      credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 't' },
      capture: makeFakeCapture(),
    })

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()
    harness.emit(makeRoomClosedEvent({ classroomId: 'room-1' }))
    await flushPromises()

    expect(session.phase).toBe('closed')
    expect(classrooms.items[0]?.status).toBe('CLOSED')
    expect(classrooms.current?.status).toBe('CLOSED')
    // 别间课堂关掉时不动我自己的会话。
    expect(session.isClosedByTeacher).toBe(true)
  })

  it('屏幕类事件只交给在课会话：SCREEN_LOST 让会话进入屏幕中断，SCREEN_RESTORED 再恢复', async () => {
    const { useMediaSessionStore } = await import('../media-session.ts')
    const session = useMediaSessionStore()
    session.prepare({
      sessionId: 'session-1',
      classroomId: 'room-1',
      credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 't' },
      capture: makeFakeCapture(),
    })
    session.phase = 'online'

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()

    harness.emit(makeScreenLostEvent('session-1'))
    expect(session.phase).toBe('screen-lost')
    expect(session.screenLostByServer).toBe(true)

    harness.emit(makeScreenRestoredEvent('session-1'))
    // 本地轨道还在 → 服务端确认恢复后回到正常显示。
    expect(session.phase).toBe('online')
    expect(session.screenLostByServer).toBe(false)
  })

  it('§25/§31：PRIVATE_TALK_* 只交给在课会话，并且报告里只有老师自己（§26）', async () => {
    const { useMediaSessionStore } = await import('../media-session.ts')
    const session = useMediaSessionStore()
    session.prepare({
      sessionId: 'session-1',
      classroomId: 'room-1',
      credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 't' },
      capture: makeFakeCapture(),
    })
    session.phase = 'online'

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()

    harness.emit(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))
    expect(session.hasTalkRequest).toBe(true)
    expect(session.talkRequestTitle).toBe('王老师希望与你进行语音沟通。')

    harness.emit(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))
    expect(session.isTalkingWithTeacher).toBe(true)

    harness.emit(makePrivateTalkEndedEvent({ sessionId: 'session-1' }))
    expect(session.isTalkingWithTeacher).toBe(false)
  })

  it('§26：老师那版的 PRIVATE_TALK_STARTED 到了学生端也不会被接受', async () => {
    const { useMediaSessionStore } = await import('../media-session.ts')
    const session = useMediaSessionStore()
    session.prepare({
      sessionId: 'session-1',
      classroomId: 'room-1',
      credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 't' },
      capture: makeFakeCapture(),
    })
    session.phase = 'online'

    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()

    // 老师版载荷（目标学生的身份，没有老师姓名）：学生端**没有**老师名字可显示，
    // 也绝不该从别的字段猜一个（§26 的"没有字段就无从推断"）。
    harness.emit(
      makeTeacherShapedPrivateTalkStartedEvent({
        studentId: 'student-1',
        sessionId: 'session-1',
        displayName: '张三',
      }),
    )

    expect(session.privateTalk).toBeNull()
    expect(session.isTalkingWithTeacher).toBe(false)
    expect(realtime.state).toBe('open')
  })

  it('不可信报文被忽略，不会打断通道（§47 的"解析失败只计数"）', () => {
    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()

    harness.emitRaw('{ 这不是 JSON')
    harness.emitRaw(JSON.stringify({ type: 'STUDENT_ONLINE', at: 'x', data: {} }))

    expect(realtime.state).toBe('open')
    expect(harness.current().closeCalls).toBe(0)
  })

  it('stop：关闭底层连接并复位状态（登出/卸载不留悬挂通道）', () => {
    const realtime = useRealtimeStore()
    realtime.start()
    harness.open()

    realtime.stop()

    expect(harness.current().closeCalls).toBe(1)
    expect(realtime.state).toBe('closed')
    expect(realtime.started).toBe(false)
    harness.current().serverClose()
    expect(harness.sockets).toHaveLength(1)
  })
})
