import { ApiError } from '@classwatch/api-client'
import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  installFakeMonitorRoom,
  makeMonitorStudent,
  makeOfflineStudent,
  makeScreenLostStudent,
} from '../../__tests__/monitor-fixtures.ts'
import { useMonitorStore } from '../monitor.ts'

/**
 * 监督 store 测试（§29 / §51 / §52）。
 *
 * 这一份盯的是"老师到底会不会看到画面、会不会看到不该看到的画面"：
 *
 * 1. 只订阅 `screen.active` 的学生（§52：绝不自动订阅全班）；
 * 2. 同一个 participant **不重复订阅**（每 10 秒刷新一次也不行）；
 * 3. 学生停止共享后要**取消订阅**（省带宽的那一步，而不是留着一条死轨道）；
 * 4. 媒体失败有重试，且重试会重新申请凭据并重建订阅；
 * 5. 业务状态只来自 monitor DTO（§51）——断线不清空数据、不靠 participant 推断在线。
 */

const { getMonitorMock, requestMediaTokenMock } = vi.hoisted(() => ({
  getMonitorMock: vi.fn(),
  requestMediaTokenMock: vi.fn(),
}))

vi.mock('../../lib/teacher-monitor-api.ts', () => ({
  getMonitor: getMonitorMock,
  requestMediaToken: requestMediaTokenMock,
}))

const MEDIA_TOKEN = {
  livekitUrl: 'wss://classwatch-test.livekit.cloud',
  token: 'teacher-token',
}

describe('监督 store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    getMonitorMock.mockReset().mockResolvedValue([makeMonitorStudent()])
    requestMediaTokenMock.mockReset().mockResolvedValue(MEDIA_TOKEN)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('load：申请 media-token → 连接（autoSubscribe 由适配层保证）→ 拉 monitor → 订阅屏幕', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()

    await store.load('room-1')

    expect(requestMediaTokenMock).toHaveBeenCalledWith('room-1')
    expect(harness.current().connectCalls).toBe(1)
    expect(store.students).toHaveLength(1)
    expect(store.loaded).toBe(true)
    // sessionId 就是订阅用的 identity（§44/§51）。
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
    expect(store.subscribedCount).toBe(1)
  })

  it('§52：只订阅 screen.active 的学生，未连接 / 屏幕中断的不订', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a'] })
    // 媒体层里只有 session-a；屏幕中断 / 未连接的那两个即便有轨道也不该被订阅。
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeScreenLostStudent({ sessionId: 'session-lost' }),
      makeOfflineStudent({ sessionId: null }),
    ])
    const store = useMonitorStore()

    await store.load('room-1')

    expect(harness.current().subscribeCalls).toEqual(['session-a'])
    expect(store.subscribedCount).toBe(1)
  })

  it('§52：同一个 participant 在多次刷新后**不会重复订阅**', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')

    // 监督墙每 10 秒刷新一次；"屏幕还在共享"每次都会成立。
    await store.refresh()
    await store.refresh()
    await store.refresh()

    expect(harness.current().subscribeCalls).toEqual(['session-1'])
    expect(getMonitorMock).toHaveBeenCalledTimes(4)
  })

  it('§52：并发刷新时同一 identity 也只会订阅一次（pending 守卫）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')

    // 同一轮里手动再协调两次：第二次必须被 pending / subscriptions 挡住。
    await Promise.all([store.reconcileSubscriptions(), store.reconcileSubscriptions()])

    expect(harness.current().subscribeCalls).toEqual(['session-1'])
  })

  it('学生停止共享：取消订阅并停止下行（不是留一条死轨道）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')
    expect(store.subscribedCount).toBe(1)

    getMonitorMock.mockResolvedValue([makeScreenLostStudent({ sessionId: 'session-1' })])
    await store.refresh()

    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(store.subscribedCount).toBe(0)
    expect(store.mediaStateOf(makeScreenLostStudent({ sessionId: 'session-1' }))).toBe('none')
  })

  it('学生从列表里消失（离开课堂）：同样取消订阅', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')

    getMonitorMock.mockResolvedValue([])
    await store.refresh()

    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(store.isEmpty).toBe(true)
  })

  it('§22：学生重新共享后（participants 事件）重新订阅上画面', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    getMonitorMock.mockResolvedValue([makeScreenLostStudent({ sessionId: 'session-1' })])
    await store.load('room-1')
    expect(harness.current().subscribeCalls).toEqual([])

    // 学生点了「重新共享整个屏幕」：业务状态与媒体轨道都会重新出现。
    getMonitorMock.mockResolvedValue([makeMonitorStudent({ sessionId: 'session-1' })])
    await store.refresh()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])

    harness.current().emitScreenSubscribed('session-1')
    expect(store.mediaStateOf(makeMonitorStudent())).toBe('subscribed')
  })

  it('订阅失败：该卡片进入 failed，并可通过重试恢复', async () => {
    const harness = installFakeMonitorRoom()
    // 失败开关要在房间创建之前打开（房间是 load 时懒创建的）。
    harness.flags.subscribeError = new Error('subscribe rejected')
    const store = useMonitorStore()

    await store.load('room-1')

    expect(store.mediaStateOf(makeMonitorStudent())).toBe('failed')
    expect(store.subscribedCount).toBe(0)

    harness.flags.subscribeError = null
    await store.refresh()
    await store.reconcileSubscriptions()

    expect(store.mediaStateOf(makeMonitorStudent())).toBe('subscribed')
  })

  it('媒体连接失败：明确的中文提示 + 不影响业务数据（不白屏）', async () => {
    installFakeMonitorRoom({ connectError: new Error('signal failed') })
    const store = useMonitorStore()

    await store.load('room-1')

    expect(store.mediaConnected).toBe(false)
    expect(store.mediaError).toContain('无法连接课堂的媒体服务器')
    // §51：业务状态来自 DTO，媒体连不上照样要显示"谁在上课"。
    expect(store.students).toHaveLength(1)
  })

  it('重试：重新申请凭据、重连、并重建订阅', async () => {
    const harness = installFakeMonitorRoom({ connectError: new Error('signal failed') })
    const store = useMonitorStore()
    await store.load('room-1')
    expect(store.mediaError).not.toBeNull()

    harness.flags.connectError = null
    await store.retryMedia()

    expect(requestMediaTokenMock).toHaveBeenCalledTimes(2)
    expect(store.mediaError).toBeNull()
    expect(store.mediaConnected).toBe(true)
    expect(harness.rooms[1]?.subscribeCalls).toEqual(['session-1'])
  })

  it('§51：参与者从房间里消失（ParticipantDisconnected）→ 立刻丢掉订阅，不等下一轮轮询', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')
    expect(store.subscriptionOf(makeMonitorStudent())).not.toBeNull()

    // 媒体层说人走了（学生关掉页面/断网），但业务状态要等下一次轮询才会更新。
    harness.current().participants = []
    harness.current().emitParticipantsChanged()
    await flushPromises()

    expect(store.subscriptionOf(makeMonitorStudent())).toBeNull()
    expect(store.mediaStateOf(makeMonitorStudent())).toBe('none')
    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    // 业务状态没有被媒体事件改写（§51）：屏还是后端说了算。
    expect(store.students).toHaveLength(1)
  })

  it('媒体断线事件：整体标红并给重试提示，但不清空学生数据', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')

    harness.current().emitDisconnected()

    expect(store.mediaError).toContain('媒体连接已经中断')
    expect(store.students).toHaveLength(1)
  })

  it('monitor 接口失败：保留上一轮数据 + 记录错误（把画面抹掉会让老师以为学生都掉线了）', async () => {
    installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')

    getMonitorMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    await store.refresh()

    expect(store.error?.code).toBe('NETWORK_ERROR')
    expect(store.students).toHaveLength(1)
  })

  it('§49 风格的低频轮询：10 秒拉一次 monitor（Phase 8 由 WebSocket 取代）', async () => {
    vi.useFakeTimers()
    installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')
    const initialCalls = getMonitorMock.mock.calls.length

    await vi.advanceTimersByTimeAsync(10_000)
    expect(getMonitorMock.mock.calls.length).toBe(initialCalls + 1)

    await vi.advanceTimersByTimeAsync(20_000)
    expect(getMonitorMock.mock.calls.length).toBe(initialCalls + 3)
  })

  it('stop：停止轮询、断开媒体、清空状态（离开页面不留连接）', async () => {
    vi.useFakeTimers()
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')

    await store.stop()
    const callsAfterStop = getMonitorMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(30_000)

    expect(getMonitorMock.mock.calls.length).toBe(callsAfterStop)
    expect(harness.current().disconnectCalls).toBe(1)
    expect(store.students).toHaveLength(0)
    expect(store.mediaConnected).toBe(false)
  })

  it('§44：凭据不进响应式 state（老师端的 token 同样只放内存）', async () => {
    installFakeMonitorRoom()
    const store = useMonitorStore()
    await store.load('room-1')

    const serialized = JSON.stringify(store.$state)
    expect(serialized).not.toContain('teacher-token')
    expect(serialized).not.toContain('wss://')
  })
})
