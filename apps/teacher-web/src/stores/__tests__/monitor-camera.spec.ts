import { flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  installFakeMonitorRoom,
  installFakeVisibility,
  makeDisconnectedStudent,
  makeLeftStudent,
  makeMonitorStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  makeCameraChangedEvent,
  makeRoomClosedEvent,
  makeStudentOfflineEvent,
} from '../../__tests__/realtime-fixtures.ts'
import { useMonitorStore } from '../monitor.ts'

/**
 * 摄像头订阅协调测试（§24 / §29 / §52 / §75）。
 *
 * Phase 9 的全部风险都集中在"**沿用** Phase 7 那套协调器"这一句话上，
 * 所以这一份测试逐条盯着：
 *
 * 1. 摄像头跟着同一套可见性 / Focus / 页面隐藏走（不另起一套策略）；
 * 2. 同一个 participant 不重复订阅；滚动来滚动去也不会反复 setSubscribed；
 * 3. **摄像头与屏幕互不影响**：退掉一边绝不能顺手拆掉另一边；
 * 4. 学生离线 / 参与者离开 / 课堂关闭时摄像头订阅被释放（不留僵死的画中画）；
 * 5. `CAMERA_CHANGED` 只改 `camera` 一个字段，并且通过协调器改订阅。
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

const MEDIA_TOKEN = {
  livekitUrl: 'wss://classwatch-test.livekit.cloud',
  token: 'teacher-token',
}

type MonitorStore = ReturnType<typeof useMonitorStore>

let pinia: Pinia

/** 摄像头开着的学生（默认连屏幕一起在线）。 */
function cameraStudent(overrides: Parameters<typeof makeMonitorStudent>[0] = {}) {
  return makeMonitorStudent({ camera: { active: true }, ...overrides })
}

function markVisible(store: MonitorStore, ...studentIds: string[]): void {
  for (const studentId of studentIds) store.setStudentVisible(studentId, true)
}

describe('监督 store — 摄像头画中画订阅（§24/§52）', () => {
  beforeEach(() => {
    pinia = createPinia()
    setActivePinia(pinia)
    getMonitorMock.mockReset().mockResolvedValue([cameraStudent()])
    requestMediaTokenMock.mockReset().mockResolvedValue(MEDIA_TOKEN)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('§24/§29：摄像头开着的可见卡片会被订阅（画中画才有画面）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')

    await store.load('room-1')

    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).not.toBeNull()
    expect(store.cameraMediaStateOf(store.students[0]!)).toBe('subscribed')
    // 屏幕那条照常，两条互不干扰。
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
  })

  it('§24：摄像头没开就不订（DTO 说了算，不看媒体层有没有轨道）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([makeMonitorStudent({ camera: { active: false } })])
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')

    await store.load('room-1')

    expect(harness.current().subscribeCameraCalls).toEqual([])
    expect(store.cameraSubscriptionOf(store.students[0]!)).toBeNull()
  })

  it('§52：摄像头跟着**同一套**可见性策略走（不在视口里就不订）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      cameraStudent({ studentId: 'a', sessionId: 'session-a' }),
      cameraStudent({ studentId: 'b', sessionId: 'session-b' }),
    ])
    const store = useMonitorStore(pinia)
    markVisible(store, 'b')

    await store.load('room-1')

    expect(harness.current().subscribeCameraCalls).toEqual(['session-b'])
  })

  it('§52：卡片滚出视口 → 摄像头订阅被取消；滚回来 → 重新订上', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])

    store.setStudentVisible('student-1', false)
    await flushPromises()

    expect(harness.current().unsubscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).toBeNull()

    store.setStudentVisible('student-1', true)
    await flushPromises()
    expect(harness.current().subscribeCameraCalls).toEqual(['session-1', 'session-1'])
  })

  it('§52：反复刷新（十秒一轮）不会重复订阅同一条摄像头轨道', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    await store.refresh()
    await store.refresh()

    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).not.toBeNull()
  })

  it('§52：页面隐藏 → 摄像头下行一起停；回到页面 → 恢复', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    store.setPageHidden(true)
    await flushPromises()

    expect(harness.current().unsubscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).toBeNull()
    // 屏幕那条同样被停（老师端隐藏页面即降载，§52）。
    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])

    store.setPageHidden(false)
    await flushPromises()
    expect(harness.current().subscribeCameraCalls).toEqual(['session-1', 'session-1'])
  })

  it('§30：Focus 的学生即使卡片不在视口里，摄像头也会被订阅', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    await store.load('room-1')
    expect(harness.current().subscribeCameraCalls).toEqual([])

    store.setFocusedStudent('student-1')
    await flushPromises()

    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])
  })

  it('§24/§52：摄像头与屏幕互不影响——退掉摄像头不会碰屏幕那条订阅', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    // 学生关掉摄像头（事件）→ 只退摄像头。
    store.applyRealtimeEvent(makeCameraChangedEvent({ active: false }))
    await flushPromises()

    expect(harness.current().unsubscribeCameraCalls).toEqual(['session-1'])
    expect(harness.current().unsubscribeCalls).toEqual([])
    expect(store.subscriptionOf(store.students[0]!)).not.toBeNull()
    expect(store.subscribedCount).toBe(1)
  })

  it('§47/§24：CAMERA_CHANGED 只改 camera 一个字段（别的字段事件证明不了）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    const before = store.students[0]!

    store.applyRealtimeEvent(makeCameraChangedEvent({ active: true }))

    const after = store.students[0]!
    expect(after.camera).toEqual({ active: true })
    // 事件载荷里没有这些东西，一个都不许被"顺手"改掉（§80）。
    expect(after.connection).toBe(before.connection)
    expect(after.joinedAt).toBe(before.joinedAt)
    expect(after.sessionStatus).toBe(before.sessionStatus)
    expect(after.screen).toEqual(before.screen)
    expect(after.displayName).toBe(before.displayName)
    expect(after.sessionId).toBe(before.sessionId)
  })

  it('§47：CAMERA_CHANGED(active=true) 之后画中画订阅被建立（走的是协调器）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([makeMonitorStudent({ camera: { active: false } })])
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(harness.current().subscribeCameraCalls).toEqual([])

    store.applyRealtimeEvent(makeCameraChangedEvent({ active: true }))
    await flushPromises()

    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraMediaStateOf(store.students[0]!)).toBe('subscribed')
  })

  it('§24：业务说他开着、媒体里还没有轨道 → 不返回订阅（界面不画空白小窗）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    // 先连上媒体并拿到名单，但卡片还没进视口（= 还没订阅任何轨道）。
    await store.load('room-1')
    expect(harness.current().subscribeCameraCalls).toEqual([])
    // 这一刻媒体层里还没有这条轨道（webhook 已经说他开了，SFU 的轨道还在路上）。
    harness.current().cameraAvailable = false

    markVisible(store, 'student-1')
    await flushPromises()

    // 尝试过订阅，但拿不到轨道：状态保持 'none'，界面因此不画那个小窗。
    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).toBeNull()
    expect(store.cameraMediaStateOf(store.students[0]!)).toBe('none')
  })

  it('摄像头订阅失败：只影响画中画，屏幕与其它卡片照常', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    harness.flags.subscribeCameraError = new Error('camera subscribe failed')

    await store.load('room-1')

    expect(store.cameraMediaStateOf(store.students[0]!)).toBe('failed')
    // 屏幕订阅用的是另一条路径，一次都没有失败过。
    expect(store.subscriptionOf(store.students[0]!)).not.toBeNull()
    expect(store.subscribedCount).toBe(1)
  })

  it('§47 STUDENT_OFFLINE：摄像头订阅被释放（不留僵死的画中画）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    store.applyRealtimeEvent(
      makeStudentOfflineEvent({ studentId: 'student-1', sessionId: 'session-1', reason: 'LEFT' }),
    )
    await flushPromises()

    expect(harness.current().unsubscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).toBeNull()
    // 事件本身也把摄像头状态置假（DTO 里同时存在的字段）。
    expect(store.students[0]?.camera.active).toBe(false)
  })

  it('§51：参与者从房间里消失 → 立刻丢掉摄像头订阅，不等下一轮轮询', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(store.cameraSubscriptionOf(store.students[0]!)).not.toBeNull()

    harness.current().participants = []
    harness.current().emitParticipantsChanged()
    await flushPromises()

    expect(harness.current().unsubscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).toBeNull()
  })

  it('§52：学生断开（DISCONNECTED）→ 摄像头订阅同样释放', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([cameraStudent()])
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    getMonitorMock.mockResolvedValue([
      makeDisconnectedStudent({ studentId: 'student-1', sessionId: 'session-1' }),
    ])
    await store.refresh()

    expect(harness.current().unsubscribeCameraCalls).toEqual(['session-1'])
    expect(store.cameraSubscriptionOf(store.students[0]!)).toBeNull()
  })

  it('§49 ROOM_CLOSED：摄像头订阅与屏幕一起释放（一条下行都不留）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    store.applyRealtimeEvent(makeRoomClosedEvent({ classroomId: 'room-1' }))

    expect(store.cameraSubscriptions).toEqual({})
    expect(store.cameraMediaStates).toEqual({})
    expect(harness.current().disconnectCalls).toBe(1)
  })

  it('离开监督墙（stop）：摄像头订阅表被清空', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')
    expect(store.cameraSubscriptionOf(store.students[0]!)).not.toBeNull()

    await store.stop()

    expect(store.cameraSubscriptions).toEqual({})
    expect(store.cameraMediaStates).toEqual({})
  })

  it('§29：画中画的画质永远 LOW，且**没有**画质切换这回事', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')
    await store.load('room-1')

    // 进出 Focus 会改**屏幕**那条的画质（§30），摄像头那条一次都不该被升档：
    // 它连 setQuality 这个入口都没有（画中画与 Focus 的 Camera 区都是小窗）。
    store.setFocusedStudent('student-1')
    await flushPromises()
    store.setFocusedStudent(null)
    await flushPromises()

    const cameraSubscription = store.cameraSubscriptionOf(store.students[0]!)
    expect(Object.keys(cameraSubscription ?? {}).sort()).toEqual(['attach', 'identity'])
    // 而且全程只订阅了一次（Focus 进出不重订摄像头）。
    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])
    // 屏幕那条确实换了档（说明上面那次 Focus 真的生效了）。
    expect(harness.current().qualityChanges).toEqual([
      { identity: 'session-1', quality: 'high' },
      { identity: 'session-1', quality: 'low' },
    ])
  })

  it('已离开名单的学生不会被订阅摄像头（哪怕 DTO 里 camera 还是 true）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeLeftStudent({ studentId: 'student-1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const store = useMonitorStore(pinia)
    markVisible(store, 'student-1')

    await store.load('room-1')

    expect(harness.current().subscribeCameraCalls).toEqual([])
  })
})
