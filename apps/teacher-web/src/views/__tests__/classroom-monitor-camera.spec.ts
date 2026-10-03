import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenClassroom } from '../../__tests__/fixtures.ts'
import {
  installFakeMonitorRoom,
  installFakeVisibility,
  makeLeftStudent,
  makeMonitorStudent,
  makeNotJoinedStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  installFakeRealtimeSocket,
  makeCameraChangedEvent,
  makeStudentOfflineEvent,
} from '../../__tests__/realtime-fixtures.ts'
import { useMonitorStore } from '../../stores/monitor.ts'
import { useRealtimeStore } from '../../stores/realtime.ts'
import { routes } from '../../router'
import ClassroomMonitorView from '../ClassroomMonitorView.vue'

/**
 * 监督墙上的摄像头画中画（§24 / §29 / §30 / §75）。
 *
 * 这一份只测**界面上的呈现**，与 store 那份（monitor-camera.spec.ts）分工明确：
 * store 测"该不该订、订了几次、退没退"，这里测"老师到底看到了什么"。
 *
 * 最关键的一条反面断言：**没有画面时不能存在那个小窗**。
 * 空白框会被读成"学生把摄像头关了"，而真实原因可能是老师这边还没订上——
 * 这两种情况的处理完全不同（一个不用管，一个再等一会儿就好）。
 */

const { getMonitorMock, requestMediaTokenMock, getClassroomMock } = vi.hoisted(() => ({
  getMonitorMock: vi.fn(),
  requestMediaTokenMock: vi.fn(),
  getClassroomMock: vi.fn(),
}))

vi.mock('../../lib/teacher-monitor-api.ts', () => ({
  getMonitor: getMonitorMock,
  requestMediaToken: requestMediaTokenMock,
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

let pinia: Pinia
const mounted: VueWrapper[] = []

async function mountView() {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/teacher/classrooms/room-1/monitor')
  await router.isReady()
  const wrapper = mount(ClassroomMonitorView, { global: { plugins: [pinia, router] } })
  mounted.push(wrapper)
  await flushPromises()
  return { wrapper, router }
}

function tileOf(wrapper: VueWrapper, studentId: string) {
  return wrapper.find(`[data-testid="monitor-tile"][data-student-id="${studentId}"]`)
}

/** 从页面上的卡片打开某人的 Focus（与老师真实操作一致）。 */
async function openFocus(wrapper: VueWrapper, studentId: string): Promise<void> {
  await tileOf(wrapper, studentId).trigger('click')
  await flushPromises()
}

describe('课堂监督墙 — 摄像头画中画（§29/§30）', () => {
  beforeEach(() => {
    pinia = createPinia()
    setActivePinia(pinia)
    getMonitorMock.mockReset().mockResolvedValue([makeMonitorStudent()])
    requestMediaTokenMock.mockReset().mockResolvedValue({
      livekitUrl: 'wss://classwatch-test.livekit.cloud',
      token: 'teacher-token',
    })
    getClassroomMock.mockReset().mockResolvedValue(CLASSROOM)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(async () => {
    for (const wrapper of mounted.splice(0)) wrapper.unmount()
    await flushPromises()
  })

  it('§29：camera.active=true 且订阅到位 → 卡片右下角出现画中画', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])

    const { wrapper } = await mountView()

    const tile = tileOf(wrapper, 's1')
    const pip = tile.find('[data-testid="tile-camera-pip"]')
    expect(pip.exists()).toBe(true)
    expect(tile.find('[data-testid="tile-camera-video"]').exists()).toBe(true)
    // 画面真的挂上去了（订阅的 attach 被调用），而不是一个空 <video>。
    expect(harness.current().cameraAttachedElements).toHaveLength(1)
  })

  it('§29：camera.active=false → 不显示画中画（也**不**留一个空框）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: false } }),
    ])

    const { wrapper } = await mountView()

    const tile = tileOf(wrapper, 's1')
    expect(tile.find('[data-testid="tile-camera-pip"]').exists()).toBe(false)
    expect(tile.find('[data-testid="tile-camera-video"]').exists()).toBe(false)
    // 页脚那行角标也不该出现：DTO 说没开。
    expect(tile.find('[data-testid="tile-camera-flag"]').exists()).toBe(false)
  })

  it('§29：说他开着但媒体里没有这条轨道 → 仍然**没有**小窗（不给空白框）', async () => {
    // 媒体层里根本没有这个 participant（webhook 说他开了摄像头，SFU 的轨道还在路上）。
    const harness = installFakeMonitorRoom({ participants: [] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])

    const { wrapper } = await mountView()

    // 试过了（说明协调器真的在按 DTO 尝试）：拿不到轨道就不记录订阅，下一轮还会再试。
    expect(harness.current().subscribeCameraCalls).toContain('session-1')
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(false)
    // 业务角标照旧显示"已开启"：这是 DTO 的事实，与画面到没到是两件事。
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-flag"]').exists()).toBe(true)
  })

  it('§29：只有订阅到位的卡片才有小窗，其它卡片不受影响', async () => {
    installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 'a', sessionId: 'session-a', camera: { active: true } }),
      makeMonitorStudent({ studentId: 'b', sessionId: 'session-b', camera: { active: false } }),
    ])

    const { wrapper } = await mountView()

    expect(tileOf(wrapper, 'a').find('[data-testid="tile-camera-pip"]').exists()).toBe(true)
    expect(tileOf(wrapper, 'b').find('[data-testid="tile-camera-pip"]').exists()).toBe(false)
  })

  it('§24：小窗和屏幕画面都显式 muted（两条轨道都不能外放声音形成回声）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])

    const { wrapper } = await mountView()

    const pip = tileOf(wrapper, 's1').find('[data-testid="tile-camera-video"]')
    const element = pip.element as HTMLVideoElement
    expect(element.autoplay).toBe(true)
    expect(element.muted).toBe(true)
    expect(pip.attributes('playsinline')).toBeDefined()
  })

  it('§29：DTO 说他开着、媒体还没到时页脚仍然显示"摄像头已开启"（业务事实）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])

    const { wrapper } = await mountView()

    // 角标来自 DTO，小窗来自订阅：前者是"他开着了"，后者是"我看到画面了"。
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-flag"]').text()).toContain(
      '摄像头已开启',
    )
  })

  it('§47：CAMERA_CHANGED(active=true) 到达时小窗当场出现（不必等下一次快照）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: false } }),
    ])
    const { wrapper } = await mountView()
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(false)

    const realtime = useRealtimeStore()
    const socket = installFakeRealtimeSocket()
    realtime.start()
    socket.open()
    await flushPromises()
    socket.emit(makeCameraChangedEvent({ studentId: 's1', sessionId: 'session-1', active: true }))
    await flushPromises()

    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(true)
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-flag"]').exists()).toBe(true)
  })

  it('§47：CAMERA_CHANGED(active=false) 到达时小窗当场消失（不留最后一帧）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const { wrapper } = await mountView()
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(true)

    const realtime = useRealtimeStore()
    const socket = installFakeRealtimeSocket()
    realtime.start()
    socket.open()
    await flushPromises()
    socket.emit(makeCameraChangedEvent({ studentId: 's1', sessionId: 'session-1', active: false }))
    await flushPromises()

    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(false)
  })

  it('§47：CAMERA_CHANGED 只改 camera —— 徽章、连接、屏幕画面都保持原样', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({
        studentId: 's1',
        sessionId: 'session-1',
        displayName: '张三',
        connection: 'FAIR',
        camera: { active: false },
      }),
    ])
    const { wrapper } = await mountView()
    const before = useMonitorStore(pinia).students[0]!

    useMonitorStore(pinia).applyRealtimeEvent(
      makeCameraChangedEvent({ studentId: 's1', sessionId: 'session-1', active: true }),
    )
    await flushPromises()

    const tile = tileOf(wrapper, 's1')
    // 徽章仍是"正常"（摄像头不影响会话状态）、连接仍是"一般"、屏幕画面还在。
    expect(tile.find('[data-testid="tile-badge"]').text()).toContain('正常')
    expect(tile.find('[data-testid="tile-connection"]').text()).toContain('一般')
    expect(tile.find('[data-testid="tile-video"]').exists()).toBe(true)
    const after = useMonitorStore(pinia).students[0]!
    expect(after.connection).toBe(before.connection)
    expect(after.joinedAt).toBe(before.joinedAt)
    expect(after.sessionStatus).toBe(before.sessionStatus)
    expect(after.screen).toEqual(before.screen)
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
  })

  it('§24：学生下线时小窗随之消失（订阅被释放，不留僵死的画中画）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const { wrapper } = await mountView()
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(true)

    useMonitorStore(pinia).applyRealtimeEvent(
      makeStudentOfflineEvent({ studentId: 's1', sessionId: 'session-1', reason: 'LEFT' }),
    )
    await flushPromises()

    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(false)
  })

  it('§29：未进入课堂的卡片没有任何摄像头痕迹', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent({ studentId: 's2', camera: { active: true } }),
    ])

    const { wrapper } = await mountView()

    const tile = tileOf(wrapper, 's2')
    expect(tile.find('[data-testid="tile-camera-pip"]').exists()).toBe(false)
    expect(tile.find('[data-testid="tile-camera-flag"]').exists()).toBe(false)
  })

  /* ------------------------------------------------------------------------ */
  /* §30：Focus View 的 Camera 区                                             */
  /* ------------------------------------------------------------------------ */

  it('§30：学生开着摄像头 → Focus 的 Camera 区显示真实画面（同一条订阅）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const { wrapper } = await mountView()

    await openFocus(wrapper, 's1')

    const focus = wrapper.find('[data-testid="monitor-focus"]')
    expect(focus.find('[data-testid="focus-camera-video"]').exists()).toBe(true)
    expect(focus.find('[data-testid="focus-camera-status"]').exists()).toBe(false)
    // 展开 Focus **不会**让 SFU 再推一路：还是那一条订阅（只是多挂了一个元素）。
    expect(harness.current().subscribeCameraCalls).toEqual(['session-1'])
    expect(harness.current().cameraAttachedElements).toHaveLength(2)
  })

  it('§30：学生没开摄像头 → Camera 区写"未开启"，而不是一个黑框', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: false } }),
    ])
    const { wrapper } = await mountView()

    await openFocus(wrapper, 's1')

    const focus = wrapper.find('[data-testid="monitor-focus"]')
    expect(focus.find('[data-testid="focus-camera-video"]').exists()).toBe(false)
    expect(focus.find('[data-testid="focus-camera-status"]').text()).toContain('未开启')
    // Camera ●/○ 指示如实反映 DTO。
    expect(focus.find('[data-testid="focus-device-camera"]').text()).toContain('未开启')
  })

  it('§30：Focus 的 Camera ●/○ 跟着 camera.active 走（开着就是已开启）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const { wrapper } = await mountView()

    await openFocus(wrapper, 's1')

    expect(
      wrapper.find('[data-testid="monitor-focus"] [data-testid="focus-device-camera"]').text(),
    ).toContain('已开启')
  })

  it('§30：摄像头订阅失败 → Camera 区说明原因，屏幕大画面照常', async () => {
    const harness = installFakeMonitorRoom()
    // 在房间被创建之前就把摄像头这条路径的失败打开（房间是懒创建的）。
    harness.flags.subscribeCameraError = new Error('camera failed')
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const { wrapper } = await mountView()

    // 卡片上不给小窗（没有画面可播）。
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(false)

    await openFocus(wrapper, 's1')

    const focus = wrapper.find('[data-testid="monitor-focus"]')
    expect(focus.find('[data-testid="focus-camera-video"]').exists()).toBe(false)
    expect(focus.find('[data-testid="focus-camera-status"]').text()).toContain('订阅失败')
    // 屏幕那条完全不受影响：大画面照旧。
    expect(focus.find('[data-testid="focus-video"]').exists()).toBe(true)
  })

  it('§30：已离开课堂的学生 Focus 里 Camera 区也说未开启（没有画面可言）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeLeftStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const { wrapper } = await mountView()

    await openFocus(wrapper, 's1')

    const focus = wrapper.find('[data-testid="monitor-focus"]')
    expect(focus.find('[data-testid="focus-camera-video"]').exists()).toBe(false)
    expect(focus.find('[data-testid="focus-camera-status"]').text()).toContain('未开启')
  })

  it('§52：小窗随卡片滚出视口一起消失（订阅被取消，画面不再下行）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', sessionId: 'session-1', camera: { active: true } }),
    ])
    const { wrapper } = await mountView()
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(true)

    useMonitorStore(pinia).setStudentVisible('s1', false)
    await flushPromises()

    expect(harness.current().unsubscribeCameraCalls).toEqual(['session-1'])
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-camera-pip"]').exists()).toBe(false)
  })
})
