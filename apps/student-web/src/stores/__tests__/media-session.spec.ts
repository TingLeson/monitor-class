import { ApiError } from '@classwatch/api-client'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeFakeCapture } from '../../__tests__/screen-fixtures'
import { installFakePublisherRoom, makeJoinResponse } from '../../__tests__/media-fixtures'
import { makeOpenStudentClassroom, makeStudentClassroom } from '../../__tests__/fixtures'
import { CLASSROOM_STATUS_POLL_MS } from '../../lib/media-session-state.ts'

/**
 * 课堂会话 store 测试（§20 / §21 / §22 / §43 / §49 / §52）。
 *
 * 这一份测试盯的是**生命周期**，最容易写错也最贵的几条：
 *
 * 1. publish 的必须**就是** Phase 5 那条轨道（同一条对象，而不是一条新的）；
 * 2. 轨道 ended 之后**不能** republish 同一条轨道，只能进 SCREEN_LOST 等学生重新共享；
 * 3. 重新共享**不重新 join**（会话还在，只是轨道没了）；
 * 4. 重试走的是**重新 join**（旧 token 是短时凭据，复用它只会再失败一次）；
 * 5. 课堂被关闭由**轮询**发现，并立刻断开媒体、停止捕获（§49）。
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

/** 控制台错误 spy：既静音失败路径的日志，也用来断言日志里没有凭据。 */
let consoleErrorSpy: ReturnType<typeof vi.spyOn>

/** 走完 PreJoin 的交接：prepare → begin。返回 store 与它手里的捕获对象。 */
async function startSession(options: { pollReturnsClosed?: boolean } = {}) {
  const { useMediaSessionStore } = await import('../media-session.ts')
  const store = useMediaSessionStore()
  const capture = makeFakeCapture()
  store.prepare({
    sessionId: 'session-1',
    classroomId: 'room-open',
    credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'fake-token' },
    capture,
  })
  getClassroomMock.mockResolvedValue(
    options.pollReturnsClosed === true
      ? makeStudentClassroom({ id: 'room-open', status: 'CLOSED' })
      : OPEN,
  )
  await store.begin()
  return { store, capture }
}

describe('课堂会话 store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    joinClassroomMock.mockReset().mockResolvedValue(makeJoinResponse())
    leaveSessionMock.mockReset().mockResolvedValue(undefined)
    getClassroomMock.mockReset().mockResolvedValue(OPEN)
    /**
     * 失败路径会往控制台写一行错误名（排障用）。测试里静音它，顺便**断言**
     * 那行日志里不含凭据：token 只允许存在于内存 store 里（§44）。
     */
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('没有 prepare 就 begin：进入 no-session（刷新页面后的必然结果，不是故障）', async () => {
    const { useMediaSessionStore } = await import('../media-session.ts')
    const store = useMediaSessionStore()

    await store.begin()

    expect(store.phase).toBe('no-session')
    expect(store.hasNoSession).toBe(true)
  })

  it('begin：连接后 publish 的正是 Phase 5 的那条轨道（同一条对象，§20）', async () => {
    const harness = installFakePublisherRoom()
    const { store, capture } = await startSession()

    expect(store.phase).toBe('online')
    expect(store.isOnline).toBe(true)
    expect(harness.rooms).toHaveLength(1)
    expect(harness.current().connectCalls).toBe(1)
    // 关键断言：发布的是**同一条** MediaStreamTrack（而不是重新采集出来的一条）。
    expect(harness.current().publishedTracks).toEqual([capture.track])
    expect(harness.credentials[0]?.token).toBe('fake-token')
  })

  it('连接成功后会立刻拉一次课堂详情（课堂名要马上显示，§49 的轮询起点）', async () => {
    installFakePublisherRoom()
    const { store } = await startSession()

    expect(getClassroomMock).toHaveBeenCalledWith('room-open')
    expect(store.classroom?.name).toBe('C++ 算法训练')
  })

  it('连接失败：media-error + 中文提示 + 保留轨道（重试不该再弹一次授权）', async () => {
    installFakePublisherRoom({ connectError: new Error('signal connection failed') })
    const { store, capture } = await startSession()

    expect(store.phase).toBe('media-error')
    expect(store.canRetry).toBe(true)
    expect(store.failure?.kind).toBe('connect')
    expect(store.failure?.message).toContain('无法连接课堂的媒体服务器')
    // 轨道没有被停：Gate 已经通过，学生重试时不需要重新选择共享范围。
    expect(capture.track.readyState).toBe('live')
    // §44：日志里只有错误名，绝不含 token。
    const logged = consoleErrorSpy.mock.calls.flat().join(' ')
    expect(logged).toContain('connect failed')
    expect(logged).not.toContain('fake-token')
  })

  it('publish 失败：错误态说明是"发布失败"而不是"连不上"', async () => {
    installFakePublisherRoom({ publishError: new Error('publish rejected') })
    const { store } = await startSession()

    expect(store.phase).toBe('media-error')
    expect(store.failure?.kind).toBe('publish')
    expect(store.failure?.message).toContain('屏幕共享没有成功发布到课堂')
  })

  it('重试：重新 join 拿新 token，再用同一条轨道连上（§43/§44）', async () => {
    const harness = installFakePublisherRoom({ connectError: new Error('boom') })
    const { store, capture } = await startSession()
    expect(store.canRetry).toBe(true)

    // 第二次连接成功（把失败开关关掉：它作用于**之后创建**的房间），
    // join 返回新的 session。
    harness.flags.connectError = null
    joinClassroomMock.mockResolvedValue(
      makeJoinResponse({ sessionId: 'session-2', token: 'fresh-token' }),
    )

    await store.retry()

    expect(joinClassroomMock).toHaveBeenCalledTimes(1)
    expect(store.phase).toBe('online')
    expect(store.sessionId).toBe('session-2')
    // 用了新的凭据，而且发布的仍是原来那条轨道。
    expect(harness.credentials[1]?.token).toBe('fresh-token')
    expect(harness.rooms[1]?.publishedTracks).toEqual([capture.track])
  })

  it('重试时 join 再次失败：rejoin 失败态，不假装在线', async () => {
    installFakePublisherRoom({ connectError: new Error('boom') })
    const { store } = await startSession()

    joinClassroomMock.mockRejectedValue(new ApiError({ code: 'MEDIA_TOKEN_FAILED', status: 502 }))
    await store.retry()

    expect(store.phase).toBe('media-error')
    expect(store.failure?.kind).toBe('rejoin')
    expect(store.canRetry).toBe(true)
  })

  it('§22：轨道 ended → screen-lost，且**不** republish 已死的轨道', async () => {
    const harness = installFakePublisherRoom()
    const { store, capture } = await startSession()
    expect(harness.current().publishedTracks).toHaveLength(1)

    capture.emitEnded()
    await Promise.resolve()

    expect(store.phase).toBe('screen-lost')
    expect(store.isScreenLost).toBe(true)
    // 一条已结束的轨道不会再产生任何一帧：再 publish 只会得到永远黑屏的"在线"学生。
    expect(harness.current().publishedTracks).toHaveLength(1)
    expect(harness.current().unpublishCalls).toBe(1)
    expect(capture.track.readyState).toBe('ended')
  })

  it('§22：重新共享 → publish 新轨道，且**不**重新 join', async () => {
    const harness = installFakePublisherRoom()
    const { store, capture } = await startSession()

    capture.emitEnded()
    await Promise.resolve()
    expect(store.phase).toBe('screen-lost')

    const fresh = makeFakeCapture()
    await store.publishCapture(fresh)

    expect(store.phase).toBe('online')
    // 只连过一次：会话还在，rejoin 会平白让老师那端的学生卡片闪断。
    expect(harness.current().connectCalls).toBe(1)
    expect(harness.current().publishedTracks).toEqual([capture.track, fresh.track])
    expect(joinClassroomMock).not.toHaveBeenCalled()
  })

  it('重新共享后再断：新轨道的 ended 同样被监听（监听要跟着轨道走）', async () => {
    installFakePublisherRoom()
    const { store, capture } = await startSession()

    capture.emitEnded()
    await Promise.resolve()
    const fresh = makeFakeCapture()
    await store.publishCapture(fresh)

    fresh.emitEnded()
    await Promise.resolve()

    expect(store.phase).toBe('screen-lost')
  })

  it('§22：轨道在 Gate 与 publish 之间就已结束 → 直接 screen-lost，不发布死轨道', async () => {
    const harness = installFakePublisherRoom()
    const { useMediaSessionStore } = await import('../media-session.ts')
    const store = useMediaSessionStore()
    const capture = makeFakeCapture()
    getClassroomMock.mockResolvedValue(OPEN)
    // 学生在 PreJoin 与进入会话页之间的几秒里点了浏览器的"停止共享"。
    capture.track.stop()
    store.prepare({
      sessionId: 'session-1',
      classroomId: 'room-open',
      credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'fake-token' },
      capture,
    })

    await store.begin()

    expect(store.phase).toBe('screen-lost')
    expect(harness.rooms).toHaveLength(0)
  })

  it('§22：轨道在 publish 飞行期间结束 → 回到 screen-lost，绝不宣布 online', async () => {
    const harness = installFakePublisherRoom()
    // publish 成功返回，但轨道在那之前已经被浏览器结束。
    harness.flags.endTrackOnPublish = true
    const { store } = await startSession()

    expect(store.phase).toBe('screen-lost')
    expect(store.isOnline).toBe(false)
  })

  it('连接质量事件更新界面用的 quality（§56 的网络行）', async () => {
    const harness = installFakePublisherRoom()
    const { store } = await startSession()

    harness.current().emitQuality('poor')
    expect(store.quality).toBe('poor')

    harness.current().emitQuality('excellent')
    expect(store.quality).toBe('excellent')
  })

  it('媒体连接意外断开：media-error（不是 left，也不是假装的 online）', async () => {
    const harness = installFakePublisherRoom()
    const { store } = await startSession()

    harness.current().emitDisconnected()

    expect(store.phase).toBe('media-error')
    expect(store.failure?.kind).toBe('disconnected')
  })

  it('自动重连：reconnecting 标记置位，重连成功后回到 online 并清掉失败提示（§52）', async () => {
    const harness = installFakePublisherRoom()
    const { store } = await startSession()

    harness.current().emitReconnecting()
    expect(store.reconnecting).toBe(true)
    expect(store.phase).toBe('online')

    harness.current().emitDisconnected()
    harness.current().emitReconnected()

    expect(store.reconnecting).toBe(false)
    expect(store.phase).toBe('online')
    expect(store.hasFailure).toBe(false)
  })

  it('离开：先上报 leave，再断开媒体、停止捕获（§49 的退出顺序）', async () => {
    const harness = installFakePublisherRoom()
    const { store, capture } = await startSession()

    await store.leave()

    expect(leaveSessionMock).toHaveBeenCalledWith('session-1')
    expect(harness.current().disconnectCalls).toBe(1)
    expect(capture.track.readyState).toBe('ended')
    expect(store.phase).toBe('left')
  })

  it('leave 上报失败也要完成本地退出（卡在网络上的学生必须能走掉）', async () => {
    const harness = installFakePublisherRoom()
    const { store, capture } = await startSession()

    leaveSessionMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    await store.leave()

    expect(store.phase).toBe('left')
    expect(harness.current().disconnectCalls).toBe(1)
    expect(capture.track.readyState).toBe('ended')
  })

  it('leave 只上报一次（离开按钮 + 组件卸载都会调它）', async () => {
    installFakePublisherRoom()
    const { store } = await startSession()

    await store.leave()
    await store.leave()

    expect(leaveSessionMock).toHaveBeenCalledTimes(1)
  })

  it('§44：leave 之后凭据被丢弃，再次 begin 不会复用旧会话', async () => {
    installFakePublisherRoom()
    const { store } = await startSession()

    await store.leave()
    await store.begin()

    // phase 已经是 left，begin 不做任何事；即便做了，凭据也已经清空。
    expect(store.phase).toBe('left')
  })

  it('§49 轮询：课堂变成 CLOSED → 断开媒体 + 停止捕获 + closed 状态', async () => {
    vi.useFakeTimers()
    const harness = installFakePublisherRoom()
    const { store, capture } = await startSession()
    expect(store.phase).toBe('online')

    getClassroomMock.mockResolvedValue(makeStudentClassroom({ id: 'room-open', status: 'CLOSED' }))
    await vi.advanceTimersByTimeAsync(CLASSROOM_STATUS_POLL_MS)

    expect(store.phase).toBe('closed')
    expect(store.isClosedByTeacher).toBe(true)
    expect(harness.current().disconnectCalls).toBe(1)
    expect(capture.track.readyState).toBe('ended')
    // 老师关课堂时后端已经把会话标记成 ROOM_CLOSED（§49），不再调 leave。
    expect(leaveSessionMock).not.toHaveBeenCalled()
  })

  it('§49 轮询：一次网络失败不改变课堂状态（抖动不等于课堂关了）', async () => {
    vi.useFakeTimers()
    installFakePublisherRoom()
    const { store, capture } = await startSession()

    getClassroomMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    await vi.advanceTimersByTimeAsync(CLASSROOM_STATUS_POLL_MS)

    expect(store.phase).toBe('online')
    expect(capture.track.readyState).toBe('live')
  })

  it('轮询在离开之后停止（否则离开的学生会一直打后端）', async () => {
    vi.useFakeTimers()
    installFakePublisherRoom()
    const { store } = await startSession()
    await store.leave()

    getClassroomMock.mockClear()
    await vi.advanceTimersByTimeAsync(CLASSROOM_STATUS_POLL_MS * 3)

    expect(getClassroomMock).not.toHaveBeenCalled()
  })

  it('releaseNow（beforeunload 的同步兜底）：断开 + 停止捕获，不做异步上报', async () => {
    const harness = installFakePublisherRoom()
    const { store, capture } = await startSession()

    store.releaseNow()

    expect(harness.current().disconnectCalls).toBe(1)
    expect(capture.track.readyState).toBe('ended')
    expect(store.phase).toBe('left')
    // beforeunload 里发请求会被浏览器取消：那一步交给服务端的 webhook 兜底（§45/§46）。
    expect(leaveSessionMock).not.toHaveBeenCalled()
  })

  it('reset：清空一切，下一个课堂不会继承上一个课堂的定时器与轨道', async () => {
    vi.useFakeTimers()
    installFakePublisherRoom()
    const { store, capture } = await startSession()

    store.reset()
    getClassroomMock.mockClear()
    await vi.advanceTimersByTimeAsync(CLASSROOM_STATUS_POLL_MS * 2)

    expect(store.phase).toBe('idle')
    expect(store.sessionId).toBeNull()
    expect(store.classroom).toBeNull()
    expect(capture.track.readyState).toBe('ended')
    expect(getClassroomMock).not.toHaveBeenCalled()
  })
})
