import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  installFakeMonitorRoom,
  installFakeVisibility,
  makeMonitorStudent,
  makeNotJoinedStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  makePrivateTalkEndedEvent,
  makeRoomClosedEvent,
  makeStudentOfflineEvent,
  makeTeacherPrivateTalkStartedEvent,
} from '../../__tests__/realtime-fixtures.ts'
import {
  installTeacherMediaDevices,
  makeTeacherMediaDevicesStub,
  micTrackOf,
} from '../../__tests__/media-fixtures.ts'
import { useMonitorStore } from '../monitor.ts'

/**
 * 老师端私密语音状态机测试（§27 / §31 / §76）。
 *
 * 这一段的核心是"**同一时刻只有一个目标**"，以及"界面永远知道自己在对谁讲话"：
 *
 * 1. POST 之后界面必须跟着**服务端**返回的 target（不是本地拼的那份名单）；
 * 2. 对另一个人再 POST = 切换，旧目标立刻不再高亮；
 * 3. DELETE = 结束；
 * 4. `TEACHER_MIC_REQUIRED` 要变成"请先开启你的麦克风"+ 一键开麦（§31 的可执行提示）；
 * 5. `PRIVATE_TALK_STARTED / ENDED` 事件与 POST/GET 的响应必须收敛到同一个状态；
 * 6. 目标学生离开 / 课堂结束 → 回到"无目标"。
 */

const {
  getMonitorMock,
  requestMediaTokenMock,
  startPrivateTalkMock,
  stopPrivateTalkMock,
  getPrivateTalkMock,
} = vi.hoisted(() => ({
  getMonitorMock: vi.fn(),
  requestMediaTokenMock: vi.fn(),
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

const MEDIA_TOKEN = {
  livekitUrl: 'wss://classwatch-test.livekit.cloud',
  token: 'teacher-token',
}

/** API 错误：`ApiError` 的形状就是 store 判断的东西（code + status）。 */
async function apiError(code: string, status: number) {
  const { ApiError } = await import('@classwatch/api-client')
  return new ApiError({ code: code as never, message: code, status })
}

let pinia: Pinia

/** 两个都在课堂里的学生（切换目标的场景）。 */
function twoStudents() {
  return [
    makeMonitorStudent({ studentId: 's1', displayName: '张三', sessionId: 'session-1' }),
    makeMonitorStudent({ studentId: 's2', displayName: '李四', sessionId: 'session-2' }),
  ]
}

describe('监督 store — 私密语音与老师麦克风（§27/§31）', () => {
  beforeEach(() => {
    pinia = createPinia()
    setActivePinia(pinia)
    getMonitorMock.mockReset().mockResolvedValue(twoStudents())
    requestMediaTokenMock.mockReset().mockResolvedValue(MEDIA_TOKEN)
    startPrivateTalkMock.mockReset()
    stopPrivateTalkMock.mockReset().mockResolvedValue(undefined)
    getPrivateTalkMock.mockReset().mockResolvedValue(null)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('§31：发起语音沟通 → POST {studentId}，界面按**响应**里的 target 高亮', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    await store.startTalk(store.students[0]!)

    expect(startPrivateTalkMock).toHaveBeenCalledWith('room-1', 's1')
    expect(store.talkTarget).toEqual({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    expect(store.isTalkTarget(store.students[0]!)).toBe(true)
    expect(store.isTalkTarget(store.students[1]!)).toBe(false)
    expect(store.talkFailure).toBeNull()
  })

  it('§31：切换目标 = 对另一个人再 POST（不先 DELETE），旧目标立刻不再高亮', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock
      .mockResolvedValueOnce({ studentId: 's1', displayName: '张三', sessionId: 'session-1' })
      .mockResolvedValueOnce({ studentId: 's2', displayName: '李四', sessionId: 'session-2' })
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    await store.startTalk(store.students[0]!)
    await store.startTalk(store.students[1]!)

    // 前端不发 DELETE：§31 要求"先撤销旧的"由服务端在设置新目标时完成，
    // 多发一次只会在中间留下一个"没有目标"的窗口。
    expect(stopPrivateTalkMock).not.toHaveBeenCalled()
    expect(startPrivateTalkMock).toHaveBeenNthCalledWith(2, 'room-1', 's2')
    expect(store.isTalkTarget(store.students[0]!)).toBe(false)
    expect(store.isTalkTarget(store.students[1]!)).toBe(true)
  })

  it('§31：结束语音沟通 → DELETE，目标清空', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.startTalk(store.students[0]!)

    await store.stopTalk()

    expect(stopPrivateTalkMock).toHaveBeenCalledWith('room-1')
    expect(store.talkTarget).toBeNull()
    expect(store.isTalkTarget(store.students[0]!)).toBe(false)
  })

  it('§31：未进入课堂的学生（sessionId === null）**不发请求**（按钮本来就是禁用的）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([makeNotJoinedStudent({ studentId: 's1' })])
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    await store.startTalk(store.students[0]!)

    expect(startPrivateTalkMock).not.toHaveBeenCalled()
    expect(store.talkTarget).toBeNull()
  })

  it('§31：TEACHER_MIC_REQUIRED → 可读提示 + micRequired（界面据此给一键开麦）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockRejectedValue(await apiError('TEACHER_MIC_REQUIRED', 409))
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    await store.startTalk(store.students[0]!)

    expect(store.talkFailure?.micRequired).toBe(true)
    expect(store.talkFailure?.message).toContain('请先开启你的麦克风')
    expect(store.talkTarget).toBeNull()
  })

  it('§31：PRIVATE_TALK_UNAVAILABLE / CLASSROOM_NOT_OWNER 各有各的可读文案', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    startPrivateTalkMock.mockRejectedValue(await apiError('PRIVATE_TALK_UNAVAILABLE', 409))
    await store.startTalk(store.students[0]!)
    expect(store.talkFailure?.message).toContain('不在课堂中')
    expect(store.talkFailure?.micRequired).toBe(false)

    startPrivateTalkMock.mockRejectedValue(await apiError('CLASSROOM_NOT_OWNER', 403))
    await store.startTalk(store.students[0]!)
    expect(store.talkFailure?.message).toContain('创建老师')
  })

  it('§31：找不到这个学生（404 STUDENT_NOT_ASSIGNED）也给一句能照做的话', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockRejectedValue(await apiError('STUDENT_NOT_ASSIGNED', 404))
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    await store.startTalk(store.students[0]!)

    expect(store.talkFailure?.message).toContain('名单')
  })

  it('§31：切换到另一个学生时清掉上一条错误（错误属于**上一个人**）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock
      .mockRejectedValueOnce(await apiError('PRIVATE_TALK_UNAVAILABLE', 409))
      .mockResolvedValueOnce({ studentId: 's2', displayName: '李四', sessionId: 'session-2' })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.startTalk(store.students[0]!)
    expect(store.talkFailure).not.toBeNull()

    await store.startTalk(store.students[1]!)

    expect(store.talkFailure).toBeNull()
    expect(store.talkTarget?.studentId).toBe('s2')
  })

  it('§31：PRIVATE_TALK_STARTED（老师版）也能建立目标（另一个标签页发起的）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    store.applyRealtimeEvent(
      makeTeacherPrivateTalkStartedEvent({
        studentId: 's2',
        sessionId: 'session-2',
        displayName: '李四',
      }),
    )

    expect(store.talkTarget?.studentId).toBe('s2')
    expect(store.isTalkTarget(store.students[1]!)).toBe(true)
  })

  it('§26：学生版 PRIVATE_TALK_STARTED（只有老师名）不会被老师端当成目标', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    store.applyRealtimeEvent({
      type: 'PRIVATE_TALK_STARTED',
      at: '2026-10-03T10:00:00Z',
      data: { teacherDisplayName: '王老师' },
    })

    expect(store.talkTarget).toBeNull()
  })

  it('§31：PRIVATE_TALK_ENDED 只结束匹配的那个目标', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.startTalk(store.students[0]!)

    // 别的会话结束了：不是我的目标，界面不该变。
    store.applyRealtimeEvent(makePrivateTalkEndedEvent({ sessionId: 'session-other' }))
    expect(store.talkTarget).not.toBeNull()

    store.applyRealtimeEvent(makePrivateTalkEndedEvent({ sessionId: 'session-1' }))
    expect(store.talkTarget).toBeNull()
  })

  it('§31：目标学生离开 → 界面立刻回到"无目标"（不等后端那条 ENDED）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.startTalk(store.students[0]!)

    store.applyRealtimeEvent(makeStudentOfflineEvent({ studentId: 's1', sessionId: 'session-1' }))

    expect(store.talkTarget).toBeNull()
  })

  it('§49：课堂结束 → 私密语音状态清空', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.startTalk(store.students[0]!)

    store.applyRealtimeEvent(makeRoomClosedEvent({ classroomId: 'room-1' }))

    expect(store.talkTarget).toBeNull()
    expect(store.classroomClosed).toBe(true)
  })

  it('§31：目标从名单里消失（快照刷新）→ 回到"无目标"', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.startTalk(store.students[0]!)

    // 学生被移出课堂：快照是名单的权威。
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's2', sessionId: 'session-2' }),
    ])
    await store.refresh()
    await flushPromises()

    expect(store.talkTarget).toBeNull()
    expect(harness.current()).toBeDefined()
  })

  it('§31：进入监督墙时恢复服务端的当前目标（刷新页面后界面不能"忘记"）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getPrivateTalkMock.mockResolvedValue({
      studentId: 's2',
      displayName: '李四',
      sessionId: 'session-2',
    })
    const store = useMonitorStore(pinia)

    await store.load('room-1')
    await flushPromises()

    expect(getPrivateTalkMock).toHaveBeenCalledWith('room-1')
    expect(store.talkTarget?.studentId).toBe('s2')
  })

  it('§31：恢复失败（后端不可达）保持"无目标"，而且不影响监督数据', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getPrivateTalkMock.mockRejectedValue(new Error('offline'))
    const store = useMonitorStore(pinia)

    await store.load('room-1')
    await flushPromises()

    expect(store.talkTarget).toBeNull()
    expect(store.students).toHaveLength(2)
  })

  /* -------------------------------------------------------------------- */
  /* §27：老师自己的麦克风                                                 */
  /* -------------------------------------------------------------------- */

  it('§27：开麦 = getUserMedia(audio) + 发布 microphone 轨道', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const devices = makeTeacherMediaDevicesStub()
    installTeacherMediaDevices(devices)
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    await store.enableTeacherMicrophone()

    expect(devices.micCalls).toHaveLength(1)
    expect(devices.micCalls[0]?.constraints).toEqual({ audio: true })
    expect(store.teacherMicState).toBe('on')
    expect(harness.current().publishedMicrophoneTracks).toHaveLength(1)
  })

  it('§27：关麦 = unpublish + 真的 stop（麦克风指示灯必须灭）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const devices = makeTeacherMediaDevicesStub()
    installTeacherMediaDevices(devices)
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.enableTeacherMicrophone()

    store.disableTeacherMicrophone()

    expect(harness.current().unpublishMicrophoneCalls).toBe(1)
    expect(micTrackOf(devices).readyState).toBe('ended')
    expect(store.teacherMicState).toBe('off')
  })

  it('§27：权限被拒 → 可执行的中文提示（不影响监督墙本身）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const devices = makeTeacherMediaDevicesStub()
    const denied = new Error('denied')
    denied.name = 'NotAllowedError'
    devices.micFailWith = denied
    installTeacherMediaDevices(devices)
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    await store.enableTeacherMicrophone()

    expect(store.teacherMicState).toBe('error')
    expect(store.teacherMicFailure).toContain('权限')
    // 监督数据一点都没变（§33：控制面与媒体面分离）。
    expect(store.students).toHaveLength(2)
  })

  it('§27/§31：开麦成功会清掉那条"请先开启你的麦克风"提示', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    startPrivateTalkMock.mockRejectedValue(await apiError('TEACHER_MIC_REQUIRED', 409))
    installTeacherMediaDevices(makeTeacherMediaDevicesStub())
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.startTalk(store.students[0]!)
    expect(store.talkFailure?.micRequired).toBe(true)

    await store.enableTeacherMicrophone()

    expect(store.talkFailure).toBeNull()
  })

  it('§31：媒体连接断开 → 老师麦克风被释放，目标也清空（不可能再讲话了）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const devices = makeTeacherMediaDevicesStub()
    installTeacherMediaDevices(devices)
    startPrivateTalkMock.mockResolvedValue({
      studentId: 's1',
      displayName: '张三',
      sessionId: 'session-1',
    })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    await store.enableTeacherMicrophone()
    await store.startTalk(store.students[0]!)

    harness.current().emitDisconnected()

    expect(store.teacherMicState).toBe('off')
    expect(micTrackOf(devices).readyState).toBe('ended')
    expect(store.talkTarget).toBeNull()
    expect(store.mediaError).not.toBeNull()
  })
})
