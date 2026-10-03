import { ApiError } from '@classwatch/api-client'
import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  installFakeMonitorRoom,
  installFakeVisibility,
  makeDisconnectedStudent,
  makeLeftStudent,
  makeMonitorStudent,
  makeNotJoinedStudent,
  makeRoomClosedStudent,
  makeScreenLostStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  makeCameraChangedEvent,
  makeRoomClosedEvent,
  makeScreenLostEvent,
  makeScreenRestoredEvent,
  makeStudentOfflineEvent,
  makeStudentOnlineEvent,
} from '../../__tests__/realtime-fixtures.ts'
import { MONITOR_DEGRADED_POLL_MS, MONITOR_FALLBACK_POLL_MS } from '../../lib/monitor-status.ts'
import { useMonitorStore } from '../monitor.ts'

/**
 * 监督 store 测试（§29 / §30 / §51 / §52）。
 *
 * 这一份盯的是"老师到底会不会看到画面、会不会看到**多余的**画面"：
 *
 * 1. 只订阅在线的、屏幕在发布的、**且在视口里**的学生（§52：绝不自动订阅全班）；
 * 2. 同一个 participant **不重复订阅**（每 10 秒刷新一次也不行）；
 * 3. 不可见 / 断开 / 离开的学生要**取消订阅**（省带宽的那一步，而不是留着一条死轨道）；
 * 4. 画质：网格 low、Focus high、退出 Focus 回 low（§30 / §52）；
 * 5. 页面不可见时停掉全部下行，回到页面再恢复；
 * 6. 媒体失败有重试，且重试会重新申请凭据并重建订阅；
 * 7. 业务状态只来自 monitor DTO（§51）——断线不清空数据、不靠 participant 推断在线；
 * 8. Phase 8 起快照之外还有**实时事件增量**（§47）：上线/下线/屏幕中断与恢复，
 *    以及"事件里出现名单外的人要忽略 + 记日志，绝不凭空造卡片"。
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

type MonitorStore = ReturnType<typeof useMonitorStore>

/**
 * 本次用例自己的 Pinia。
 *
 * WHY 显式持有并传给 `useMonitorStore(pinia)`，而不是只依赖 `setActivePinia`：
 * Pinia 在**求值 getter** 时会执行 `setActivePinia(自己的 pinia)`，
 * 于是任何一个仍然活着的旧 store 只要被读一次 getter，就会把模块级的 activePinia
 * 改回旧实例；下一个用例的 `useMonitorStore(pinia)` 便会拿到上一个用例的 store，
 * 表现是"假媒体房间明明装了却没人调用它"这种极难定位的串测。
 */
let pinia: Pinia

/**
 * 让若干张卡片进入视口（§52 的输入）。
 *
 * Phase 7 起"可见"是订阅的必要条件，所以几乎每个用例都要先摆好这个前提；
 * 显式写出来也让"为什么这个人没有被订阅"在测试里一眼可见。
 */
function markVisible(store: MonitorStore, ...studentIds: string[]): void {
  for (const studentId of studentIds) store.setStudentVisible(studentId, true)
}

describe('监督 store', () => {
  beforeEach(() => {
    pinia = createPinia()
    setActivePinia(pinia)
    getMonitorMock.mockReset().mockResolvedValue([makeMonitorStudent()])
    requestMediaTokenMock.mockReset().mockResolvedValue(MEDIA_TOKEN)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('load：申请 media-token → 连接（autoSubscribe 由适配层保证）→ 拉 monitor → 订阅可见卡片的屏幕', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')

    await store.load('room-1')

    expect(requestMediaTokenMock).toHaveBeenCalledWith('room-1')
    expect(harness.current().connectCalls).toBe(1)
    expect(store.students).toHaveLength(1)
    expect(store.loaded).toBe(true)
    // sessionId 就是订阅用的 identity（§44/§51）。
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
    expect(store.subscribedCount).toBe(1)
  })

  it('§52：只订阅在线的学生，未进入 / 屏幕中断的不订', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a'] })
    // 媒体层里只有 session-a；屏幕中断 / 未进入的那两个即便有轨道也不该被订阅。
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeScreenLostStudent({ sessionId: 'session-lost' }),
      makeNotJoinedStudent({ sessionId: null }),
    ])
    const store = useMonitorStore(pinia)
    markVisible(store, 'a', 'student-lost', 'student-not-joined')

    await store.load('room-1')

    expect(harness.current().subscribeCalls).toEqual(['session-a'])
    expect(store.subscribedCount).toBe(1)
  })

  it('§52：不在视口里的卡片不订阅（滚动到哪儿才看哪儿）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    const store = useMonitorStore(pinia)
    // 只有 b 这一张卡片在视口里（a 被滚到了屏幕外）。
    markVisible(store, 'b')

    await store.load('room-1')

    expect(harness.current().subscribeCalls).toEqual(['session-b'])
    expect(store.subscribedCount).toBe(1)
  })

  it('§52：卡片进入视口 → 订阅；离开视口 → 取消订阅（不是留着一条看不见的下行）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    expect(harness.current().subscribeCalls).toEqual([])

    // 滚到 a：订阅建立（低画质，因为它在网格里）。
    store.setStudentVisible('a', true)
    await flushPromises()
    expect(harness.current().subscribeCalls).toEqual(['session-a'])
    expect(harness.current().subscribeQualities).toEqual(['low'])

    // 再滚到 b：a 仍在视口里，两者的订阅并存。
    store.setStudentVisible('b', true)
    await flushPromises()
    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b'])

    // 滚过 a：a 的订阅被撤销，b 保留。
    store.setStudentVisible('a', false)
    await flushPromises()
    expect(harness.current().unsubscribeCalls).toEqual(['session-a'])
    expect(store.subscriptionOf(makeMonitorStudent({ sessionId: 'session-a' }))).toBeNull()
    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b'])
  })

  it('§52：同一个 participant 在多次刷新后**不会重复订阅**', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    // 监督墙每 10 秒刷新一次；"屏幕还在共享"每次都会成立。
    await store.refresh()
    await store.refresh()
    await store.refresh()

    expect(harness.current().subscribeCalls).toEqual(['session-1'])
    expect(getMonitorMock).toHaveBeenCalledTimes(4)
  })

  it('§52：一面墙的订阅请求是**并发**发出的（串行 await 会让最后几格等十几秒）', async () => {
    const harness = installFakeMonitorRoom({
      participants: ['session-a', 'session-b', 'session-c'],
    })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' }),
      makeMonitorStudent({ studentId: 'c', sessionId: 'session-c' }),
    ])
    // 闸门打开：所有订阅请求都挂住，直到我们放行。串行实现到这一步只会发出**一条**请求。
    let release: () => void = () => undefined
    harness.flags.subscribeGate = new Promise<void>((resolve) => {
      release = resolve
    })
    const store = useMonitorStore(pinia)
    markVisible(store, 'a', 'b', 'c')

    const loading = store.load('room-1')
    await flushPromises()

    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b', 'session-c'])
    expect(store.mediaStates).toMatchObject({
      'session-a': 'pending',
      'session-b': 'pending',
      'session-c': 'pending',
    })

    release()
    await loading

    expect(store.subscribedCount).toBe(3)
  })

  it('§52：并发刷新时同一 identity 也只会订阅一次（pending 守卫）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    // 同一轮里手动再协调两次：第二次必须被 pending / subscriptions 挡住。
    await Promise.all([store.reconcileSubscriptions(), store.reconcileSubscriptions()])

    expect(harness.current().subscribeCalls).toEqual(['session-1'])
  })

  /* ---------------------------------------------------------------------- */
  /* 画质分层（§30 / §52）                                                   */
  /* ---------------------------------------------------------------------- */

  it('§30/§52：网格 low → Focus high → 退出 Focus 回 low，全程**不重新订阅**', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(harness.current().subscribeQualities).toEqual(['low'])

    store.setFocusedStudent('student-1')
    await flushPromises()
    expect(harness.current().qualityChanges).toEqual([{ identity: 'session-1', quality: 'high' }])
    expect(store.qualityOf(makeMonitorStudent())).toBe('high')
    // 关键：升画质是**改档**，不是退订重订——重订会让老师看到一次黑屏。
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
    expect(harness.current().unsubscribeCalls).toEqual([])

    store.setFocusedStudent(null)
    await flushPromises()
    expect(harness.current().qualityChanges).toEqual([
      { identity: 'session-1', quality: 'high' },
      { identity: 'session-1', quality: 'low' },
    ])
    expect(store.qualityOf(makeMonitorStudent())).toBe('low')
  })

  it('§30：Focus 的学生即使卡片不在视口里也会被订阅，并直接按 high 订阅', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    expect(harness.current().subscribeCalls).toEqual([])

    store.setFocusedStudent('student-1')
    await flushPromises()

    expect(harness.current().subscribeCalls).toEqual(['session-1'])
    expect(harness.current().subscribeQualities).toEqual(['high'])
  })

  it('§52：进入 Focus 不会撤销其它可见卡片（它们还在视口里）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    const store = useMonitorStore(pinia)
    markVisible(store, 'a', 'b')
    await store.load('room-1')
    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b'])

    store.setFocusedStudent('a')
    await flushPromises()

    expect(harness.current().unsubscribeCalls).toEqual([])
    expect(store.qualityOf(makeMonitorStudent({ sessionId: 'session-a' }))).toBe('high')
    // 另一张卡片仍然是网格画质。
    expect(store.qualityOf(makeMonitorStudent({ sessionId: 'session-b' }))).toBe('low')
  })

  it('§52：每 10 秒一轮的画质重算不会反复给 SFU 发信令（档位没变就不切）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    await store.refresh()
    await store.refresh()
    store.setFocusedStudent('student-1')
    await flushPromises()
    await store.refresh()
    await store.refresh()

    expect(harness.current().qualityChanges).toEqual([{ identity: 'session-1', quality: 'high' }])
  })

  it('§30：Focus 的学生从名单里消失时，焦点自动收回（不给一个不存在的人留订阅）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    store.setFocusedStudent('student-1')
    await flushPromises()

    // 老师把学生移出了课堂名单。
    getMonitorMock.mockResolvedValue([makeNotJoinedStudent()])
    await store.refresh()

    expect(store.focusedStudentId).toBeNull()
    expect(store.focusedStudent).toBeNull()
    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
  })

  /* ---------------------------------------------------------------------- */
  /* 页面可见性（§52 / 与 §23 的区别）                                        */
  /* ---------------------------------------------------------------------- */

  it('§52：页面隐藏 → 停掉全部下行；回到页面 → 只恢复"该看的那些"', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    const store = useMonitorStore(pinia)
    markVisible(store, 'a', 'b')
    await store.load('room-1')
    expect(store.subscribedCount).toBe(2)

    // 老师切到了别的标签页：那些像素没有任何人看。
    store.setPageHidden(true)
    await flushPromises()
    expect(store.pageHidden).toBe(true)
    expect(harness.current().unsubscribeCalls).toEqual(['session-a', 'session-b'])
    expect(store.subscribedCount).toBe(0)

    // 回到页面：可见的两张卡片重新有画面。
    store.setPageHidden(false)
    await flushPromises()
    expect(store.pageHidden).toBe(false)
    expect(harness.current().subscribeCalls).toEqual([
      'session-a',
      'session-b',
      'session-a',
      'session-b',
    ])
    expect(store.subscribedCount).toBe(2)
  })

  it('§52：页面隐藏期间刷新出新学生也不会偷偷订阅（计划是空的）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
    ])
    const store = useMonitorStore(pinia)
    markVisible(store, 'a', 'b')
    await store.load('room-1')

    store.setPageHidden(true)
    await flushPromises()
    expect(store.subscribedCount).toBe(0)

    // 隐藏期间 b 也进入了课堂、也出现在视口里（轮询仍在跑）。
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    await store.refresh()
    store.setStudentVisible('b', true)
    await flushPromises()

    expect(harness.current().subscribeCalls).toEqual(['session-a'])
    expect(store.subscribedCount).toBe(0)
  })

  it('§52：隐藏期间正在建立的订阅会被随后的协调撤销（不留悬挂的下行）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    // 订阅已建立，此时页面被隐藏。
    store.setPageHidden(true)
    await flushPromises()

    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(store.mediaStateOf(makeMonitorStudent())).toBe('none')
  })

  it('§23 的区别：老师端隐藏页面会降载，学生端不会（这里只断言老师端的行为边界）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    store.setPageHidden(true)
    await flushPromises()
    // 媒体连接**没有**断开：隐藏只是停止下行，重连要恢复几百毫秒，没有必要。
    expect(store.mediaConnected).toBe(true)
    expect(harness.current().disconnectCalls).toBe(0)
  })

  /* ---------------------------------------------------------------------- */
  /* 离线 / 离开 / 名单变化                                                   */
  /* ---------------------------------------------------------------------- */

  it('学生停止共享：取消订阅并停止下行（不是留一条死轨道）', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(store.subscribedCount).toBe(1)

    getMonitorMock.mockResolvedValue([makeScreenLostStudent({ sessionId: 'session-1' })])
    await store.refresh()

    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(store.subscribedCount).toBe(0)
    expect(store.mediaStateOf(makeScreenLostStudent({ sessionId: 'session-1' }))).toBe('none')
  })

  it('§52：学生断开（DISCONNECTED）→ 订阅被释放，哪怕后端这一轮还说屏幕在发布', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(store.subscribedCount).toBe(1)

    getMonitorMock.mockResolvedValue([
      makeDisconnectedStudent({ sessionId: 'session-1', screen: { active: true } }),
    ])
    await store.refresh()

    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(store.subscribedCount).toBe(0)
  })

  it('学生从列表里消失（离开课堂）：同样取消订阅', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    getMonitorMock.mockResolvedValue([])
    await store.refresh()

    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(store.isEmpty).toBe(true)
  })

  it('课堂被关闭（ROOM_CLOSED）：所有订阅立即释放，一条下行都不留', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    const store = useMonitorStore(pinia)
    markVisible(store, 'a', 'b')
    await store.load('room-1')
    expect(store.subscribedCount).toBe(2)

    // 老师在另一个标签页点了"关闭课堂"：后端把这一 Run 的全部会话收尾成 ROOM_CLOSED。
    getMonitorMock.mockResolvedValue([
      makeRoomClosedStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeRoomClosedStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    await store.refresh()

    expect(store.subscribedCount).toBe(0)
    expect(harness.current().unsubscribeCalls).toEqual(['session-a', 'session-b'])
  })

  it('§22：学生重新共享后（participants 事件）重新订阅上画面', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
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

  /* ---------------------------------------------------------------------- */
  /* 计数（§29 的 "18 / 25"）                                                */
  /* ---------------------------------------------------------------------- */

  it('§29：头部计数 M 是名单总数、N 是已进入数——三种情形都要对', async () => {
    installFakeMonitorRoom({ participants: [] })
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    // 全部进入。
    expect(store.rosterCount).toBe(1)
    expect(store.enteredCount).toBe(1)

    // 部分进入：一个在线、一个未进入、一个已断开。
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' }),
      makeNotJoinedStudent(),
      makeDisconnectedStudent(),
    ])
    await store.refresh()
    expect(store.rosterCount).toBe(3)
    expect(store.enteredCount).toBe(1)

    // 全部未进入：人数仍然是 3，已进入是 0（这就是老师最需要的那一眼）。
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent(),
      makeNotJoinedStudent({ studentId: 'student-2' }),
      makeNotJoinedStudent({ studentId: 'student-3' }),
    ])
    await store.refresh()
    expect(store.rosterCount).toBe(3)
    expect(store.enteredCount).toBe(0)
  })

  /* ---------------------------------------------------------------------- */
  /* 失败与恢复                                                              */
  /* ---------------------------------------------------------------------- */

  it('订阅失败：该卡片进入 failed，并可通过重试恢复', async () => {
    const harness = installFakeMonitorRoom()
    // 失败开关要在房间创建之前打开（房间是 load 时懒创建的）。
    harness.flags.subscribeError = new Error('subscribe rejected')
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')

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
    const store = useMonitorStore(pinia)

    await store.load('room-1')

    expect(store.mediaConnected).toBe(false)
    expect(store.mediaError).toContain('无法连接课堂的媒体服务器')
    // §51：业务状态来自 DTO，媒体连不上照样要显示"谁在上课"。
    expect(store.students).toHaveLength(1)
  })

  it('重试：重新申请凭据、重连、并重建订阅', async () => {
    const harness = installFakeMonitorRoom({ connectError: new Error('signal failed') })
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
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
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
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
    const store = useMonitorStore(pinia)

    await store.load('room-1')
    harness.current().emitDisconnected()

    expect(store.mediaError).toContain('媒体连接已经中断')
    expect(store.students).toHaveLength(1)
  })

  it('monitor 接口失败：保留上一轮数据 + 记录错误（把画面抹掉会让老师以为学生都掉线了）', async () => {
    installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    getMonitorMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    await store.refresh()

    expect(store.error?.code).toBe('NETWORK_ERROR')
    expect(store.students).toHaveLength(1)
  })

  it('§44：会话失效（AUTH_REQUIRED）→ 释放全部订阅与媒体连接', async () => {
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(store.subscribedCount).toBe(1)

    getMonitorMock.mockRejectedValue(new ApiError({ code: 'AUTH_REQUIRED', status: 401 }))
    await store.refresh()

    expect(store.error?.code).toBe('AUTH_REQUIRED')
    expect(store.mediaConnected).toBe(false)
    expect(store.subscribedCount).toBe(0)
    expect(harness.current().disconnectCalls).toBe(1)
  })

  it('§47：实时通道正常时兜底快照放宽到 60 秒（事件才是主路径）', async () => {
    vi.useFakeTimers()
    installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    store.setRealtimeState('open')
    const initialCalls = getMonitorMock.mock.calls.length

    await vi.advanceTimersByTimeAsync(MONITOR_DEGRADED_POLL_MS)
    expect(getMonitorMock.mock.calls.length).toBe(initialCalls)

    await vi.advanceTimersByTimeAsync(MONITOR_FALLBACK_POLL_MS - MONITOR_DEGRADED_POLL_MS)
    expect(getMonitorMock.mock.calls.length).toBe(initialCalls + 1)
  })

  it('§47：实时通道断开时兜底收紧到 20 秒（这段窗口里快照是唯一的信息来源）', async () => {
    vi.useFakeTimers()
    installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    store.setRealtimeState('open')
    const initialCalls = getMonitorMock.mock.calls.length

    store.setRealtimeState('closed')
    await vi.advanceTimersByTimeAsync(MONITOR_DEGRADED_POLL_MS)

    expect(getMonitorMock.mock.calls.length).toBe(initialCalls + 1)
  })

  /* -------------------------------------------------------------------- */
  /* §47：实时事件增量                                                     */
  /* -------------------------------------------------------------------- */

  it('§47 STUDENT_ONLINE：未进入的学生变成在线，计数 + 视口内立刻订阅', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-new'] })
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent({ studentId: 'student-new', displayName: '张三', sessionId: null }),
    ])
    const store = useMonitorStore(pinia)
    // 卡片从一开始就在视口里（未进入的学生也有卡片，DOM 层的可见性与他是否在线无关）。
    markVisible(store, 'student-new')
    await store.load('room-1')
    // 未进入 → 没有订阅可言。
    expect(store.enteredCount).toBe(0)
    expect(store.badgeOf(store.students[0]!).label).toBe('未进入')

    store.applyRealtimeEvent(
      makeStudentOnlineEvent({
        studentId: 'student-new',
        displayName: '张三',
        sessionId: 'session-new',
      }),
    )
    await flushPromises()

    expect(store.enteredCount).toBe(1)
    expect(store.students[0]?.sessionStatus).toBe('ONLINE')
    expect(store.badgeOf(store.students[0]!).emoji).toBe('🟢')
    // §21 的不变量：ONLINE ⇒ 屏幕在发布。不设它会让刚进来的学生显示成"屏幕中断"。
    expect(store.students[0]?.screen.active).toBe(true)
    // 卡片本来就在视口里（可见性早就报过）→ 协调器在事件到达后直接把它订上。
    expect(harness.current().subscribeCalls).toEqual(['session-new'])
  })

  it('§47 STUDENT_OFFLINE：卡片转为已离开并释放订阅（不留下一条永远不会有画面的下行）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(store.subscribedCount).toBe(1)

    store.applyRealtimeEvent(
      makeStudentOfflineEvent({ studentId: 'student-1', sessionId: 'session-1', reason: 'LEFT' }),
    )
    await flushPromises()

    expect(store.students[0]?.sessionStatus).toBe('LEFT')
    expect(store.students[0]?.screen.active).toBe(false)
    expect(store.badgeOf(store.students[0]!).emoji).toBe('⚫')
    expect(store.enteredCount).toBe(0)
    expect(store.subscribedCount).toBe(0)
    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
  })

  it('§47 STUDENT_OFFLINE 的三种原因映射到三种状态（断线 ≠ 离开）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    store.applyRealtimeEvent(makeStudentOfflineEvent({ reason: 'DISCONNECTED' }))
    expect(store.students[0]?.sessionStatus).toBe('DISCONNECTED')
    expect(store.badgeOf(store.students[0]!).label).toBe('已断开')

    store.applyRealtimeEvent(makeStudentOfflineEvent({ reason: 'ROOM_CLOSED' }))
    expect(store.students[0]?.sessionStatus).toBe('ROOM_CLOSED')
    expect(store.badgeOf(store.students[0]!).label).toBe('已离开')
  })

  it('§22/§47 SCREEN_LOST 与 SCREEN_RESTORED：徽章跟着变，恢复后视口内重新订阅', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(store.subscribedCount).toBe(1)

    store.applyRealtimeEvent(makeScreenLostEvent())
    await flushPromises()

    expect(store.badgeOf(store.students[0]!).emoji).toBe('🔴')
    expect(store.tileStateOf(store.students[0]!)).toBe('SCREEN_LOST')
    expect(store.subscribedCount).toBe(0)
    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])

    // 学生重新共享 → 服务端说恢复了 → 协调器把画面订回来（他还在视口里）。
    store.applyRealtimeEvent(makeScreenRestoredEvent())
    await flushPromises()

    expect(store.badgeOf(store.students[0]!).emoji).toBe('🟢')
    expect(store.enteredCount).toBe(1)
    expect(harness.current().subscribeCalls).toEqual(['session-1', 'session-1'])
  })

  it('§47：名单外的学生事件被忽略并记日志，绝不凭空造卡片（缺的字段没人能补）', async () => {
    vi.useFakeTimers()
    installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined)
    const callsBefore = getMonitorMock.mock.calls.length

    store.applyRealtimeEvent(
      makeStudentOnlineEvent({ studentId: 'ghost', displayName: '陌生人', sessionId: 's-ghost' }),
    )

    // 没有新卡片，也没有任何"用事件拼出来的"状态。
    expect(store.students).toHaveLength(1)
    expect(store.students[0]?.studentId).toBe('student-1')
    expect(warn).toHaveBeenCalledTimes(1)
    expect(String(warn.mock.calls[0]?.[0])).toContain('STUDENT_ONLINE')
    // 名单只有快照说了算：安排一次（合并的）快照刷新，而不是就地编一行。
    await vi.advanceTimersByTimeAsync(500)
    expect(getMonitorMock.mock.calls.length).toBe(callsBefore + 1)
  })

  it('§47：CAMERA_CHANGED 更新卡片上的摄像头事实（Phase 9 的挂点，本 Phase 不做画面）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    store.applyRealtimeEvent(makeCameraChangedEvent({ active: true }))

    expect(store.students[0]?.camera.active).toBe(true)
    // 摄像头不参与屏幕订阅决策（§52 只管 screen）。
    expect(store.subscribedCount).toBe(0)
  })

  it('§49 ROOM_CLOSED：标记课堂结束、主动释放媒体、并把最终状态取回来', async () => {
    vi.useFakeTimers()
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(harness.current().connectCalls).toBe(1)

    store.applyRealtimeEvent(makeRoomClosedEvent({ classroomId: 'room-1' }))

    expect(store.classroomClosed).toBe(true)
    // 主动断开：等服务端终止房间会先在界面上弹一句"无法看到学生画面"，
    // 而那是关课的正常结果，不是故障。
    expect(store.mediaConnected).toBe(false)
    expect(store.subscribedCount).toBe(0)
    expect(harness.current().disconnectCalls).toBe(1)
    expect(store.mediaError).toBeNull()

    // 事件载荷里没有每个学生的最终状态 → 合并一次快照刷新。
    getMonitorMock.mockResolvedValue([makeRoomClosedStudent({ studentId: 'student-1' })])
    await vi.advanceTimersByTimeAsync(500)
    expect(store.students[0]?.sessionStatus).toBe('ROOM_CLOSED')
  })

  it('§47：事件之前发出、之后才回来的快照不会把事件结论改回去', async () => {
    vi.useFakeTimers()
    installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    // 第一份快照取的是"事件之前"的时刻（学生还在线）——真实场景里，
    // WS 重连后立刻补快照，服务端随即把排队的事件推过来。
    getMonitorMock.mockResolvedValueOnce([makeMonitorStudent({ studentId: 'student-1' })])
    getMonitorMock.mockResolvedValue([makeLeftStudent({ studentId: 'student-1' })])

    const pending = store.refresh()
    store.applyRealtimeEvent(
      makeStudentOfflineEvent({ studentId: 'student-1', sessionId: 'session-1', reason: 'LEFT' }),
    )
    await pending

    // 过期快照被丢弃：这一刻状态仍然是事件带来的"已离开"。
    expect(store.students[0]?.sessionStatus).toBe('LEFT')

    // 丢弃后安排的合并刷新会取到新的答案。
    await vi.advanceTimersByTimeAsync(500)
    expect(store.students[0]?.sessionStatus).toBe('LEFT')
  })

  it('§49：别间课堂的 ROOM_CLOSED 不影响本页', async () => {
    installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    store.applyRealtimeEvent(makeRoomClosedEvent({ classroomId: 'other-room' }))

    expect(store.classroomClosed).toBe(false)
    expect(store.mediaConnected).toBe(true)
  })

  it('stop：停止轮询、断开媒体、清空状态（离开页面不留连接，也不留下次进入的可见性）', async () => {
    vi.useFakeTimers()
    const harness = installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    store.setFocusedStudent('student-1')

    await store.stop()
    const callsAfterStop = getMonitorMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(30_000)

    expect(getMonitorMock.mock.calls.length).toBe(callsAfterStop)
    expect(harness.current().disconnectCalls).toBe(1)
    expect(store.students).toHaveLength(0)
    expect(store.mediaConnected).toBe(false)
    expect(store.focusedStudentId).toBeNull()
    expect(store.pageHidden).toBe(false)
    // 重新进入时不会带着上一次的可见性立刻订阅一批不在视口里的卡片。
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
  })

  it('§44：凭据不进响应式 state（老师端的 token 同样只放内存）', async () => {
    installFakeMonitorRoom()
    const store = useMonitorStore(pinia)
    await store.load('room-1')

    const serialized = JSON.stringify(store.$state)
    expect(serialized).not.toContain('teacher-token')
    expect(serialized).not.toContain('wss://')
  })
})
