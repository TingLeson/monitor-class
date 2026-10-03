import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { installFakePublisherRoom, makeJoinResponse } from '../../__tests__/media-fixtures'
import { makeFakeCapture } from '../../__tests__/screen-fixtures'
import {
  installMediaDevices,
  makeMediaDevicesStub,
  type FakeTrack,
  type MediaDevicesStub,
} from '../../__tests__/screen-fixtures'
import { makeOpenStudentClassroom } from '../../__tests__/fixtures'
import {
  makePrivateTalkEndedEvent,
  makePrivateTalkRequestEvent,
  makeRoomClosedEvent,
  makeStudentPrivateTalkStartedEvent,
} from '../../__tests__/realtime-fixtures.ts'

/**
 * 麦克风与私密语音 store 测试（§25 / §26 / §31 / §76）。
 *
 * 这一份盯住四件事，每一件都对应一次真实的界面撒谎风险：
 *
 * 1. **点击才请求**（§25）：挂载/进入课堂不会碰麦克风；老师的事件也**不能**自动开麦
 *    （老师无法绕过浏览器权限）。
 * 2. **关 = unpublish + stop**：只 unpublish 会让系统麦克风指示灯继续亮着。
 * 3. **麦克风与私密语音都不影响课堂状态**（§21）：失败、开关、请求提示、静音，
 *    `phase` 与屏幕轨道一个都不许动。
 * 4. **学生端只知道自己与老师**（§26）：事件载荷里没有别人的字段，界面也没有。
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

/** 进课堂（prepare + begin），并把假浏览器装好。 */
async function startSession(options: { devices?: MediaDevicesStub } = {}) {
  const { useMediaSessionStore } = await import('../media-session.ts')
  const store = useMediaSessionStore()
  const devices = options.devices ?? makeMediaDevicesStub()
  installMediaDevices(devices)
  const capture = makeFakeCapture()
  store.prepare({
    sessionId: 'session-1',
    classroomId: 'room-open',
    credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'fake-token' },
    capture,
  })
  getClassroomMock.mockResolvedValue(OPEN)
  await store.begin()
  return { store, capture, devices }
}

/** 第 N 次（默认第一次）getUserMedia 造出来的麦克风轨道。 */
function micTrackOf(devices: MediaDevicesStub, index = 0): FakeTrack {
  const track = devices.micStreams[index]?.getAudioTracks()[0]
  if (!track) throw new Error(`假浏览器没有产出第 ${index} 条麦克风轨道`)
  return track
}

describe('课堂会话 store — 麦克风（§25/§31）', () => {
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

  it('§25：进入课堂（begin）不会请求麦克风——挂载时 getUserMedia 必须是 0 次', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()

    expect(store.phase).toBe('online')
    expect(devices.micCalls).toHaveLength(0)
    expect(devices.cameraCalls).toHaveLength(0)
    expect(store.micState).toBe('off')
  })

  it('§25：点击才请求 `{audio:true}`，发布的正是那条麦克风轨道（名字/来源由适配层钉住）', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()

    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    expect(devices.micCalls).toHaveLength(1)
    // 冻结的那一行约束：只要音频，不要视频（摄像头有它自己的开关）。
    expect(devices.micCalls[0]?.constraints).toEqual({ audio: true })
    // 发布的必须是**同一条**轨道（不是重新采集出来的另一条）。
    expect(harness.current().publishedMicrophoneTracks).toEqual([micTrackOf(devices)])
    expect(harness.current().publishedMicrophoneTracks[0]?.kind).toBe('audio')
    // 屏幕那条一次都没被碰过：三条轨道互不影响。
    expect(harness.current().publishedTracks).toHaveLength(1)
    expect(store.isMicOn).toBe(true)
    expect(store.micFailure).toBeNull()
  })

  it('§25：关闭 = unpublish + 真的 stop 本地轨道（系统麦克风指示灯必须灭）', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    store.toggleMicrophone()

    expect(harness.current().unpublishMicrophoneCalls).toBe(1)
    expect(micTrackOf(devices).stopCalls).toBe(1)
    expect(micTrackOf(devices).readyState).toBe('ended')
    expect(store.micState).toBe('off')
    // 课堂状态完全不受影响（§21：屏幕才是 mandatory）。
    expect(store.phase).toBe('online')
    expect(harness.current().publishedTracks).toHaveLength(1)
  })

  it('§25：关掉之后可以再开——再开是一次**新的** getUserMedia，而不是复用死轨道', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()

    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })
    store.toggleMicrophone()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    expect(devices.micCalls).toHaveLength(2)
    expect(harness.current().publishedMicrophoneTracks).toEqual([
      micTrackOf(devices, 0),
      micTrackOf(devices, 1),
    ])
    // 第一条轨道已经死了，绝不能再发布它。
    expect(micTrackOf(devices, 0).readyState).toBe('ended')
    expect(micTrackOf(devices, 1).readyState).toBe('live')
  })

  it('§25：权限被拒 → 可执行提示（含"下一步"与"仍能听到老师讲话"），课堂状态一点都没变', async () => {
    installFakePublisherRoom()
    const { store, devices, capture } = await startSession()
    const permissionError = new Error('denied')
    permissionError.name = 'NotAllowedError'
    devices.micFailWith = permissionError

    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('error')
    })

    expect(store.micFailure?.code).toBe('MIC_PERMISSION_DENIED')
    expect(store.micFailure?.message).toContain('权限')
    expect(store.micFailure?.message).toContain('仍然可以正常上课')
    // 这几条就是"麦克风失败绝不影响课堂会话"的全部内容。
    expect(store.phase).toBe('online')
    expect(store.hasFailure).toBe(false)
    expect(capture.track.readyState).toBe('live')
    expect(store.isMicOn).toBe(false)
  })

  it('§25：设备被占用 → 提示"可能被其他程序占用"，同样不影响课堂', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    const busy = new Error('busy')
    busy.name = 'NotReadableError'
    devices.micFailWith = busy

    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('error')
    })

    expect(store.micFailure?.kind).toBe('device-busy')
    expect(store.micFailure?.message).toContain('占用')
    expect(store.phase).toBe('online')
  })

  it('publish 失败：不留下一条亮着灯的本地轨道', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    harness.flags.publishMicrophoneError = new Error('publish rejected')

    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('error')
    })

    expect(devices.micCalls).toHaveLength(1)
    expect(micTrackOf(devices).stopCalls).toBe(1)
    // 与摄像头同构：publish 失败进 error（"打不开"与"没开"是两句不同的话）。
    expect(store.micState).toBe('error')
    expect(store.micFailure).not.toBeNull()
  })

  it('§25：连点两下不会发出第二次设备请求（requesting 期间的重入保护）', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()

    store.toggleMicrophone()
    store.toggleMicrophone()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    expect(devices.micCalls).toHaveLength(1)
  })

  it('§25：还没进入课堂时不申请设备权限（没有房间可发布）', async () => {
    installFakePublisherRoom()
    const { useMediaSessionStore } = await import('../media-session.ts')
    const store = useMediaSessionStore()
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)

    expect(store.canUseMicrophone).toBe(false)
    store.toggleMicrophone()
    await Promise.resolve()

    expect(devices.micCalls).toHaveLength(0)
    expect(store.micState).toBe('off')
  })

  it('§25：麦克风中途被拔出（track ended）→ 回到 error 并释放，不留假"已开启"', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    micTrackOf(devices).emitEnded()

    expect(store.micState).toBe('error')
    expect(store.micFailure?.kind).toBe('track-ended')
    /**
     * 死轨道还必须被 unpublish：否则 SFU 里留着一条没人推流的 microphone 发布，
     * 服务端的 webhook 永远不会说"麦克风停了"，老师那端的 `microphone.active` 会一直为真。
     */
    expect(harness.current().unpublishMicrophoneCalls).toBeGreaterThanOrEqual(1)
  })

  it('§25/§65：离开课堂必须停掉麦克风（设备不能被后台占着）', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    await store.leave()

    expect(micTrackOf(devices).readyState).toBe('ended')
    expect(store.micState).toBe('off')
    expect(store.privateTalk).toBeNull()
  })

  it('§25：releaseNow（beforeunload 的同步兜底）也要同步停掉麦克风', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    store.releaseNow()

    // 卸载路径上异步断开很可能跑不完，但 stop() 是同步的——录音必须立刻停。
    expect(micTrackOf(devices).readyState).toBe('ended')
    expect(store.micState).toBe('off')
  })

  it('§49：老师关闭课堂 → 麦克风与私密语音一并释放', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })
    store.applyRealtimeEvent(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))

    store.applyRealtimeEvent(makeRoomClosedEvent({ classroomId: 'room-open' }))

    expect(store.phase).toBe('closed')
    expect(micTrackOf(devices).readyState).toBe('ended')
    expect(store.micState).toBe('off')
    expect(store.privateTalk).toBeNull()
  })

  it('§25：媒体连接断开 → 麦克风被释放（"已开启"不能在没有上行时继续显示）', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    harness.current().emitDisconnected()

    expect(store.phase).toBe('media-error')
    expect(store.micState).toBe('off')
    expect(micTrackOf(devices).readyState).toBe('ended')
  })

  /* -------------------------------------------------------------------- */
  /* §31：老师的私密语音请求                                               */
  /* -------------------------------------------------------------------- */

  it('§25：收到 PRIVATE_TALK_REQUEST 只显示提示——**不开麦**，也不碰课堂状态', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()

    store.applyRealtimeEvent(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))

    expect(store.hasTalkRequest).toBe(true)
    expect(store.talkRequestTitle).toBe('王老师希望与你进行语音沟通。')
    // 老师不能绕过浏览器权限：收到事件一次设备请求都没有发生。
    expect(devices.micCalls).toHaveLength(0)
    expect(store.micState).toBe('off')
    expect(store.phase).toBe('online')
  })

  it('§25：点「开启麦克风」才走 getUserMedia；开启成功后提示变成"正在与王老师语音沟通"', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.applyRealtimeEvent(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))

    store.acceptTalkRequest()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    expect(devices.micCalls).toHaveLength(1)
    expect(devices.micCalls[0]?.constraints).toEqual({ audio: true })
    // "还在等你决定要不要开麦"这一半必须收掉，剩余的是持续指示。
    expect(store.hasTalkRequest).toBe(false)
    expect(store.isTalkingWithTeacher).toBe(true)
    expect(store.talkActiveText).toBe('正在与王老师语音沟通')
  })

  it('§25：点「暂不开启」→ 提示关闭，但沟通继续（老师仍能单向讲话）', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.applyRealtimeEvent(makePrivateTalkRequestEvent({ teacherDisplayName: '王老师' }))

    store.declineTalk()

    expect(store.hasTalkRequest).toBe(false)
    // 关键：老师**仍然**在对我讲话（§25 的原文）。
    expect(store.isTalkingWithTeacher).toBe(true)
    expect(devices.micCalls).toHaveLength(0)
    // 拒绝之后老师再"选中我"一次也不该把提示弹回来（不重复打扰）。
    store.applyRealtimeEvent(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))
    expect(store.hasTalkRequest).toBe(false)
  })

  it('§31：PRIVATE_TALK_STARTED / ENDED 让持续指示出现与消失', async () => {
    installFakePublisherRoom()
    const { store } = await startSession()

    store.applyRealtimeEvent(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))
    expect(store.isTalkingWithTeacher).toBe(true)
    expect(store.talkActiveText).toBe('正在与王老师语音沟通')

    store.applyRealtimeEvent(makePrivateTalkEndedEvent({ sessionId: 'session-1' }))
    expect(store.isTalkingWithTeacher).toBe(false)
    expect(store.privateTalk).toBeNull()
  })

  it('§46：别的会话的 PRIVATE_TALK_ENDED 不会结束我这段沟通（重试后 sessionId 会变）', async () => {
    installFakePublisherRoom()
    const { store } = await startSession()
    store.applyRealtimeEvent(makeStudentPrivateTalkStartedEvent({ teacherDisplayName: '王老师' }))

    store.applyRealtimeEvent(makePrivateTalkEndedEvent({ sessionId: 'session-other' }))

    expect(store.isTalkingWithTeacher).toBe(true)
  })

  it('§31：老师那版载荷（带 studentId）不会被学生端当成"老师在和我讲话"', async () => {
    installFakePublisherRoom()
    const { store } = await startSession()

    // 老师版 PRIVATE_TALK_STARTED 没有 teacherDisplayName：学生端没有姓名可显示，
    // 也不该从别的字段猜一个（§26 的"没有字段就无从推断"）。
    store.applyRealtimeEvent({
      type: 'PRIVATE_TALK_STARTED',
      at: '2026-10-03T10:00:00Z',
      data: { studentId: 'student-1', sessionId: 'session-1', displayName: '张三' },
    })

    expect(store.privateTalk).toBeNull()
    expect(store.isTalkingWithTeacher).toBe(false)
  })

  it('§31：本地静音（"静音自己"）不释放设备，麦克风仍然是已开启', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleMicrophone()
    await vi.waitFor(() => {
      expect(store.micState).toBe('on')
    })

    store.toggleSelfMute()

    expect(store.micMuted).toBe(true)
    expect(store.isMicrophoneAudible).toBe(false)
    // 关键：静音 ≠ 关麦。设备还在、发布还在（老师的下行订阅不会被拆掉）。
    expect(store.micState).toBe('on')
    expect(micTrackOf(devices).readyState).toBe('live')
    expect(micTrackOf(devices).enabled).toBe(false)

    store.toggleSelfMute()
    expect(micTrackOf(devices).enabled).toBe(true)
  })

  it('§31：老师语音被浏览器拦住时状态是 blocked，点一下之后变成 playing', async () => {
    const harness = installFakePublisherRoom()
    const { store } = await startSession()

    harness.current().emitTeacherAudio('blocked')

    expect(store.teacherAudio).toBe('blocked')

    await store.resumeTeacherAudio()

    expect(harness.current().resumeAudioCalls).toBe(1)
    expect(store.teacherAudio).toBe('playing')
  })
})
