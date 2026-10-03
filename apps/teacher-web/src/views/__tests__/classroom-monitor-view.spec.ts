import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenClassroom } from '../../__tests__/fixtures.ts'
import {
  installFakeMonitorRoom,
  installFakeVisibility,
  makeConnectingStudent,
  makeDisconnectedStudent,
  makeLeftStudent,
  makeMonitorStudent,
  makeNotJoinedStudent,
  makeRoomClosedStudent,
  makeScreenLostStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  installFakeRealtimeSocket,
  makeRoomClosedEvent,
  makeScreenLostEvent,
  makeStudentOfflineEvent,
  makeStudentOnlineEvent,
} from '../../__tests__/realtime-fixtures.ts'
import { useMonitorStore } from '../../stores/monitor.ts'
import { useRealtimeStore } from '../../stores/realtime.ts'
import { routes } from '../../router'
import ClassroomMonitorView from '../ClassroomMonitorView.vue'

/**
 * 监督墙页面测试（§29 / §30 / §51 / §52 / §73）。
 *
 * 这一页存在的唯一理由是"老师真的能看到学生桌面，并且看得起"，所以断言分成四组：
 *
 * 1. **卡片由 Monitor DTO 渲染**（业务状态，§51）：名字、七种徽章、七种主体文案；
 *    名单里的**未进入**学生也必须有一张卡片——老师最想知道的就是谁还没进来；
 * 2. **画面由 LiveKit 订阅提供**（媒体状态）：只有订阅到位的卡片才有 `<video>`，
 *    而且它必须 `autoplay + playsinline + muted`——少一个 `muted`，老师一开口
 *    就会把学生桌面的声音放出来形成回声；
 * 3. **Focus**（§30）：点开大画面、右侧设备面板、Esc 退出、私密语音按钮（§31）；
 * 4. **订阅策略**（§52）：可见才订、不可见就退、网格低画质、Focus 高画质、
 *    页面隐藏即停。
 *
 * 反面断言同样重要：媒体连不上时**不能白屏**，业务状态照样要显示出来。
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

/**
 * 私密语音接口（§31）。`load()` 会读一次"当前目标"来恢复界面记忆，
 * 不替身的话每个用例都会真的去 fetch 一次（既慢又会打出连接失败的噪音）。
 */
vi.mock('../../lib/private-talk-api.ts', () => ({
  startPrivateTalk: vi.fn(),
  stopPrivateTalk: vi.fn(),
  getPrivateTalk: vi.fn().mockResolvedValue(null),
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

/**
 * 本次用例自己的 Pinia 与已挂载的视图。
 *
 * WHY 必须显式把 pinia 交给 `mount`（而不是只调 `setActivePinia`）：
 * Pinia 在**求值 getter** 时会执行 `setActivePinia(自己的 pinia)`，
 * 于是上一个用例里没卸载的视图只要有一次渲染，就会把模块级的 activePinia
 * 改回旧实例；下一个用例的 `useMonitorStore()` 便会拿到上一个用例的 store，
 * 表现是"假媒体房间明明装了却没人调用它"这种极难定位的串测。
 * 把 pinia 作为插件装进 app 后，`inject(piniaSymbol)` 一定命中本次用例的实例。
 */
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

/** 找到某个学生的卡片（按 studentId，而不是按顺序——顺序是后端的权威）。 */
function tileOf(wrapper: VueWrapper, studentId: string) {
  return wrapper.find(`[data-testid="monitor-tile"][data-student-id="${studentId}"]`)
}

/** 触发一次页面内的重新加载（等价于点"重新加载"，也等价于一次轮询）。 */
async function refreshData() {
  await useMonitorStore(pinia).refresh()
  await flushPromises()
}

/** 派发一次 Esc：面板监听的是 window，老师可能已经把焦点移到别处。 */
async function pressEscape(): Promise<void> {
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
  await flushPromises()
}

/** 让 happy-dom 报出指定的页面可见性（真实浏览器里由浏览器决定）。 */
function setDocumentVisibility(state: 'visible' | 'hidden'): void {
  Object.defineProperty(document, 'visibilityState', { value: state, configurable: true })
  document.dispatchEvent(new Event('visibilitychange'))
}

describe('课堂监督墙', () => {
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
    setDocumentVisibility('visible')
  })

  afterEach(async () => {
    /**
     * 必须卸载：视图带着媒体连接、10 秒轮询定时器与 document 监听。
     * 留着它们不只是"内存脏"，而是会让下一个用例看到上一个用例的行为。
     */
    for (const wrapper of mounted.splice(0)) wrapper.unmount()
    await flushPromises()
    setDocumentVisibility('visible')
  })

  /* ------------------------------------------------------------------------ */
  /* 头部与网格（§29 / §73）                                                  */
  /* ------------------------------------------------------------------------ */

  it('页头显示课堂名、状态、"已进入 N / 共 M"与正在监督的画面数', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="monitor-title"]').text()).toBe('课堂监督墙')
    expect(wrapper.find('[data-testid="monitor-classroom-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="monitor-classroom-status"]').text()).toContain('已开启')
    expect(wrapper.find('[data-testid="monitor-entered-count"]').text()).toBe('已进入 1 / 共 1')
    expect(wrapper.find('[data-testid="monitor-subscribed-count"]').text()).toContain('1 路画面')
  })

  it('§29：计数在"全部进入 / 部分进入 / 全部未进入"三种情形下都正确', async () => {
    installFakeMonitorRoom({ participants: ['session-a'] })
    installFakeVisibility()

    // 全部未进入：名单 3 人，一个都没进来。
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent({ studentId: 's1', displayName: '甲' }),
      makeNotJoinedStudent({ studentId: 's2', displayName: '乙' }),
      makeNotJoinedStudent({ studentId: 's3', displayName: '丙' }),
    ])
    const { wrapper } = await mountView()
    expect(wrapper.find('[data-testid="monitor-entered-count"]').text()).toBe('已进入 0 / 共 3')
    expect(wrapper.findAll('[data-testid="monitor-tile"]')).toHaveLength(3)
    expect(wrapper.find('[data-testid="monitor-nobody-entered"]').exists()).toBe(true)

    // 部分进入：一个在线、一个断开、一个未进入。
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', displayName: '甲', sessionId: 'session-a' }),
      makeDisconnectedStudent({ studentId: 's2', displayName: '乙' }),
      makeNotJoinedStudent({ studentId: 's3', displayName: '丙' }),
    ])
    await refreshData()
    expect(wrapper.find('[data-testid="monitor-entered-count"]').text()).toBe('已进入 1 / 共 3')
    expect(wrapper.find('[data-testid="monitor-nobody-entered"]').exists()).toBe(false)

    // 全部进入。
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', displayName: '甲', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 's2', displayName: '乙', sessionId: 'session-b' }),
    ])
    await refreshData()
    expect(wrapper.find('[data-testid="monitor-entered-count"]').text()).toBe('已进入 2 / 共 2')
  })

  it('§29：七种状态徽章各自渲染正确，包括"未进入"', async () => {
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', displayName: '张三', sessionId: 'session-a' }),
      makeScreenLostStudent({ studentId: 's2', displayName: '李四' }),
      makeConnectingStudent({ studentId: 's3', displayName: '赵六' }),
      makeDisconnectedStudent({ studentId: 's4', displayName: '孙七' }),
      makeLeftStudent({ studentId: 's5', displayName: '周八' }),
      makeRoomClosedStudent({ studentId: 's6', displayName: '吴九' }),
      makeNotJoinedStudent({ studentId: 's7', displayName: '王五' }),
    ])
    // 媒体层里只有"正常"的那一个（session-a）；其余本来就不该被订阅。
    installFakeMonitorRoom({ participants: ['session-a'] })
    installFakeVisibility()
    const { wrapper } = await mountView()

    const badgeOf = (studentId: string) =>
      tileOf(wrapper, studentId).find('[data-testid="tile-badge"]').text()

    expect(badgeOf('s1')).toContain('🟢 正常')
    expect(badgeOf('s2')).toContain('🔴 屏幕中断')
    expect(badgeOf('s3')).toContain('🟡 连接中')
    expect(badgeOf('s4')).toContain('⚪ 已断开')
    expect(badgeOf('s5')).toContain('⚫ 已离开')
    expect(badgeOf('s6')).toContain('⚫ 已离开')
    expect(badgeOf('s7')).toContain('⚪ 未进入')

    // 派生状态写进了 DOM（排障时截一张图就能说清当时是哪一档）。
    expect(tileOf(wrapper, 's7').attributes('data-tile-state')).toBe('NOT_JOINED')

    // 只有"正常"的那张有画面；其余给出各自的原因，而不是一个黑框。
    expect(wrapper.findAll('[data-testid="tile-video"]')).toHaveLength(1)
    expect(tileOf(wrapper, 's7').find('[data-testid="tile-placeholder"]').text()).toBe('未进入课堂')
    expect(tileOf(wrapper, 's2').find('[data-testid="tile-placeholder"]').text()).toBe('等待共享')
  })

  it('§29：未进入的学生也有一张卡片（否则"谁还没进来"就被藏起来了）', async () => {
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', displayName: '张三' }),
      makeNotJoinedStudent({ studentId: 's2', displayName: '王五' }),
    ])
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    expect(wrapper.findAll('[data-testid="monitor-tile"]')).toHaveLength(2)
    expect(tileOf(wrapper, 's2').text()).toContain('未进入课堂')
    expect(tileOf(wrapper, 's2').text()).toContain('本次课堂该学生尚未进入')
    expect(tileOf(wrapper, 's2').find('[data-testid="tile-video"]').exists()).toBe(false)
  })

  it('§29：<video> 必须 autoplay + playsinline + muted（少一个 muted 就会回声）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    const video = wrapper.find('[data-testid="tile-video"]')
    const element = video.element as HTMLVideoElement
    expect(element.autoplay).toBe(true)
    expect(element.muted).toBe(true)
    // playsinline 走 HTML 属性（浏览器会把同名 attribute 映射到 playsInline 属性；
    // happy-dom 不映射，所以这里断言 attribute 本身）。
    expect(video.attributes('playsinline')).toBeDefined()
    // 屏幕轨道没有音频，但"显式静音"是一条必须写死的约束，不能依赖默认值。
    expect(video.attributes('muted')).not.toBe('false')
    expect(video.attributes('autoplay')).not.toBe('false')
  })

  /* ------------------------------------------------------------------------ */
  /* Focus View（§30）                                                        */
  /* ------------------------------------------------------------------------ */

  it('§30：点击卡片进入 Focus：大画面 + 右侧设备面板 + 网络行', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a'] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({
        studentId: 's1',
        displayName: '张三',
        sessionId: 'session-a',
        camera: { active: true },
        microphone: { active: true },
        connection: 'FAIR',
      }),
    ])
    const { wrapper } = await mountView()
    expect(wrapper.find('[data-testid="monitor-focus"]').exists()).toBe(false)

    await tileOf(wrapper, 's1').trigger('click')
    await flushPromises()

    const focus = wrapper.find('[data-testid="monitor-focus"]')
    expect(focus.exists()).toBe(true)
    expect(focus.find('[data-testid="focus-name"]').text()).toBe('张三')
    expect(focus.find('[data-testid="focus-video"]').exists()).toBe(true)
    // 同一份订阅现在挂到了两个 <video> 上（网格小窗 + Focus 大画面）。
    expect(harness.current().attachedElements).toHaveLength(2)

    // 右侧面板：Camera 真实画面（Phase 9）、Screen / Camera / Mic、Network。
    expect(focus.find('[data-testid="focus-camera-video"]').exists()).toBe(true)
    expect(focus.find('[data-testid="focus-device-screen"]').text()).toContain('已开启')
    expect(focus.find('[data-testid="focus-device-camera"]').text()).toContain('已开启')
    expect(focus.find('[data-testid="focus-device-microphone"]').text()).toContain('已开启')
    expect(focus.find('[data-testid="focus-network"]').text()).toContain('Fair')
    // 同一条摄像头订阅现在挂在两个元素上（网格画中画 + Focus 的 Camera 区）：
    // 展开 Focus 不会让 SFU 再推一路视频（§52）。
    expect(harness.current().cameraAttachedElements).toHaveLength(2)

    // 可访问性：面板是一个（非模态）dialog，并且标出了它的名字。
    const dialog = focus.find('[role="dialog"]')
    expect(dialog.attributes('aria-label')).toBe('聚焦视图：张三')
  })

  it('§30/§52：进入 Focus 把该学生升到高画质，退出时降回低画质', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    await tileOf(wrapper, 'student-1').trigger('click')
    await flushPromises()
    expect(harness.current().qualityChanges).toEqual([{ identity: 'session-1', quality: 'high' }])

    await wrapper.find('[data-testid="focus-close"]').trigger('click')
    await flushPromises()
    expect(harness.current().qualityChanges).toEqual([
      { identity: 'session-1', quality: 'high' },
      { identity: 'session-1', quality: 'low' },
    ])
    expect(wrapper.find('[data-testid="monitor-focus"]').exists()).toBe(false)
  })

  it('§30：Esc 退出 Focus（键盘用户必须能出来）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    await tileOf(wrapper, 'student-1').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="monitor-focus"]').exists()).toBe(true)

    await pressEscape()

    expect(wrapper.find('[data-testid="monitor-focus"]').exists()).toBe(false)
    // 退出后网格还在，卡片仍然可见、仍有画面（没有重新订阅一次）。
    expect(wrapper.findAll('[data-testid="monitor-tile"]')).toHaveLength(1)
    expect(wrapper.find('[data-testid="tile-video"]').exists()).toBe(true)
  })

  it('§30：Enter / Space 也能打开 Focus（卡片是按钮，不是只能点的 div）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    const tile = tileOf(wrapper, 'student-1')
    expect(tile.attributes('role')).toBe('button')
    expect(tile.attributes('tabindex')).toBe('0')
    expect(tile.attributes('aria-label')).toContain('张三')

    await tile.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(wrapper.find('[data-testid="monitor-focus"]').exists()).toBe(true)

    await pressEscape()
    await tile.trigger('keydown', { key: ' ' })
    await flushPromises()
    expect(wrapper.find('[data-testid="monitor-focus"]').exists()).toBe(true)
  })

  it('§31：Focus 的「语音沟通」按钮在进入课堂的学生上**可用**（Phase 10 真正启用）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()
    await tileOf(wrapper, 'student-1').trigger('click')
    await flushPromises()

    const talk = wrapper.find('[data-testid="focus-talk"]')
    expect(talk.text()).toBe('语音沟通')
    expect((talk.element as HTMLButtonElement).disabled).toBe(false)
    // 旧的"Phase 10 接入"占位说明必须消失：一个能点的按钮配一句"还没做"最让人困惑。
    expect(wrapper.find('[data-testid="focus-talk-note"]').exists()).toBe(false)
  })

  it('§30：未进入的学生点开 Focus 只讲原因，不产生任何订阅（也不给空窗）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent({ studentId: 's1', displayName: '王五' }),
    ])
    const { wrapper } = await mountView()

    await tileOf(wrapper, 's1').trigger('click')
    await flushPromises()

    const focus = wrapper.find('[data-testid="monitor-focus"]')
    expect(focus.find('[data-testid="focus-placeholder"]').text()).toBe('未进入课堂')
    expect(focus.text()).toContain('本次课堂该学生尚未进入')
    expect(focus.find('[data-testid="focus-video"]').exists()).toBe(false)
    // 关键：点开一个"没有会话"的学生不会去要一条不存在的轨道。
    expect(harness.current().subscribeCalls).toEqual([])
  })

  /* ------------------------------------------------------------------------ */
  /* 订阅策略（§52）                                                          */
  /* ------------------------------------------------------------------------ */

  it('§52：只有进入视口的卡片才订阅；滚出视口立即退订', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', displayName: '甲', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 's2', displayName: '乙', sessionId: 'session-b' }),
    ])
    const visibility = installFakeVisibility()
    const { wrapper } = await mountView()

    // 两张卡片默认都在首屏（替身在 observe 时就会报告可见）。
    expect(visibility.observed).toEqual(['s1', 's2'])
    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b'])
    expect(harness.current().subscribeQualities).toEqual(['low', 'low'])

    // 老师往下滚：乙离开视口。
    visibility.setVisible('s2', false)
    await flushPromises()
    expect(harness.current().unsubscribeCalls).toEqual(['session-b'])
    expect(tileOf(wrapper, 's2').find('[data-testid="tile-video"]').exists()).toBe(false)
    // 甲还在视口里，订阅没有被牵连。
    expect(tileOf(wrapper, 's1').find('[data-testid="tile-video"]').exists()).toBe(true)

    // 滚回来：重新订阅一次（这次也只需要一次）。
    visibility.setVisible('s2', true)
    await flushPromises()
    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b', 'session-b'])
    expect(tileOf(wrapper, 's2').find('[data-testid="tile-video"]').exists()).toBe(true)
  })

  it('§52：切换 Focus 不会取消订阅其它可见卡片', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-a', 'session-b'] })
    getMonitorMock.mockResolvedValue([
      makeMonitorStudent({ studentId: 's1', displayName: '甲', sessionId: 'session-a' }),
      makeMonitorStudent({ studentId: 's2', displayName: '乙', sessionId: 'session-b' }),
    ])
    installFakeVisibility()
    const { wrapper } = await mountView()
    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b'])

    await tileOf(wrapper, 's1').trigger('click')
    await flushPromises()

    expect(harness.current().unsubscribeCalls).toEqual([])
    expect(harness.current().qualityChanges).toEqual([{ identity: 'session-a', quality: 'high' }])
    expect(harness.current().subscribeCalls).toEqual(['session-a', 'session-b'])
  })

  it('§52：老师切到别的标签页 → 停止全部下行；切回来 → 恢复可见卡片的画面', async () => {
    // 默认学生（session-1）就是假房间里的那个人。
    const harness = installFakeMonitorRoom({ participants: ['session-1'] })
    installFakeVisibility()
    const { wrapper } = await mountView()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])

    setDocumentVisibility('hidden')
    await flushPromises()
    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(wrapper.find('[data-testid="tile-video"]').exists()).toBe(false)

    setDocumentVisibility('visible')
    await flushPromises()
    expect(harness.current().subscribeCalls).toEqual(['session-1', 'session-1'])
    expect(wrapper.find('[data-testid="tile-video"]').exists()).toBe(true)
  })

  it('§52：订阅失败不会重复轰炸（每 10 秒刷新也不会重复订阅同一个 participant）', async () => {
    const harness = installFakeMonitorRoom()
    installFakeVisibility()
    await mountView()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])

    // 模拟两次轮询刷新：卡片重渲染，但订阅调用次数不变。
    getMonitorMock.mockResolvedValue([makeMonitorStudent()])
    await refreshData()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])
  })

  it('离开监督墙（组件卸载）后不再观察卡片，也不留下订阅', async () => {
    const harness = installFakeMonitorRoom()
    const visibility = installFakeVisibility()
    const { wrapper } = await mountView()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])

    wrapper.unmount()
    await flushPromises()

    expect(visibility.visible.size).toBe(0)
    expect(harness.current().disconnectCalls).toBe(1)
  })

  /* ------------------------------------------------------------------------ */
  /* 失败与边界                                                               */
  /* ------------------------------------------------------------------------ */

  it('业务说该共享、媒体还没到时显示"正在订阅画面…"（而不是假装已连接）', async () => {
    const harness = installFakeMonitorRoom()
    harness.flags.subscribeError = new Error('subscribe rejected')
    installFakeVisibility()
    const { wrapper } = await mountView()

    const placeholder = wrapper.find('[data-testid="tile-placeholder"]')
    expect(placeholder.text()).toBe('画面订阅失败')
    expect(wrapper.find('[data-testid="tile-video"]').exists()).toBe(false)
  })

  it('媒体连接失败：显示明确提示 + 重试入口，同时业务状态照常渲染（不白屏）', async () => {
    const harness = installFakeMonitorRoom({ connectError: new Error('signal failed') })
    installFakeVisibility()
    const { wrapper } = await mountView()

    const error = wrapper.find('[data-testid="monitor-media-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain('无法连接课堂的媒体服务器')
    // §51：媒体连不上，也要能看到"谁在上课"。
    expect(wrapper.findAll('[data-testid="monitor-tile"]')).toHaveLength(1)
    expect(wrapper.find('[data-testid="monitor-empty"]').exists()).toBe(false)

    harness.flags.connectError = null
    await wrapper.find('[data-testid="monitor-media-retry"]').trigger('click')
    await flushPromises()

    expect(requestMediaTokenMock).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="monitor-media-error"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="tile-video"]').exists()).toBe(true)
  })

  it('monitor 数据失败：给提示与重新加载，已有画面保留', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockRejectedValueOnce(new Error('boom'))
    const { wrapper } = await mountView()

    const error = wrapper.find('[data-testid="monitor-data-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain('监督数据没有刷新')

    getMonitorMock.mockResolvedValue([makeMonitorStudent()])
    await error.find('button').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="monitor-data-error"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="monitor-tile"]')).toHaveLength(1)
  })

  it('课堂里没有学生：显示空状态，而不是一片空白', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([])
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="monitor-empty"]').text()).toContain('还没有学生')
    expect(wrapper.find('[data-testid="monitor-grid"]').exists()).toBe(false)
  })

  it('回到课堂详情的入口一直在（不用按浏览器后退）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="monitor-back"]').attributes('href')).toBe(
      '/teacher/classrooms/room-1',
    )
  })

  it('§44：页面里不出现媒体凭据', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    expect(wrapper.html()).not.toContain('teacher-token')
    expect(wrapper.html()).not.toContain('wss://')
  })

  it('§30：Focus 是页面内状态，不新增路由（URL 不因打开 Focus 而改变）', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper, router } = await mountView()

    await tileOf(wrapper, 'student-1').trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.fullPath).toBe('/teacher/classrooms/room-1/monitor')
    expect(wrapper.find('[data-testid="monitor-focus"]').exists()).toBe(true)
  })

  /* -------------------------------------------------------------------- */
  /* §47：实时事件驱动的监督墙                                             */
  /* -------------------------------------------------------------------- */

  it('§47：实时事件到达时卡片与计数当场变化（不必等下一次快照）', async () => {
    installFakeMonitorRoom({ participants: ['session-1'] })
    const visibility = installFakeVisibility()
    getMonitorMock.mockResolvedValue([makeMonitorStudent()])
    const { wrapper } = await mountView()

    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore(pinia)
    realtime.start()
    socket.open()
    await flushPromises()

    expect(tileOf(wrapper, 'student-1').find('[data-testid="tile-badge"]').text()).toContain('正常')

    socket.emit(makeScreenLostEvent({ studentId: 'student-1', sessionId: 'session-1' }))
    await flushPromises()

    const badge = tileOf(wrapper, 'student-1').find('[data-testid="tile-badge"]')
    expect(badge.text()).toContain('屏幕中断')
    expect(tileOf(wrapper, 'student-1').attributes('data-tile-state')).toBe('SCREEN_LOST')
    // 订阅被释放（§52：断了的卡片不留一条永远不会有画面的下行）。
    expect(visibility.visible.has('student-1')).toBe(true)
  })

  it('§47：新上线的学生在视口里会被订阅（复用 Phase 7 的订阅协调器）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-new'] })
    installFakeVisibility()
    getMonitorMock.mockResolvedValue([
      makeNotJoinedStudent({ studentId: 'student-new', displayName: '张三', sessionId: null }),
    ])
    const { wrapper } = await mountView()
    // 首屏：未进入 → 没有订阅。
    expect(harness.current().subscribeCalls).toEqual([])

    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore(pinia)
    realtime.start()
    socket.open()
    await flushPromises()

    socket.emit(
      makeStudentOnlineEvent({
        studentId: 'student-new',
        displayName: '张三',
        sessionId: 'session-new',
      }),
    )
    await flushPromises()

    expect(harness.current().subscribeCalls).toEqual(['session-new'])
    expect(tileOf(wrapper, 'student-new').find('[data-testid="tile-badge"]').text()).toContain(
      '正常',
    )
    expect(wrapper.find('[data-testid="monitor-entered-count"]').text()).toContain('已进入 1')
  })

  it('§47：学生下线时订阅被释放（画面不会停在最后一帧）', async () => {
    const harness = installFakeMonitorRoom({ participants: ['session-1'] })
    installFakeVisibility()
    const { wrapper } = await mountView()
    expect(harness.current().subscribeCalls).toEqual(['session-1'])

    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore(pinia)
    realtime.start()
    socket.open()
    await flushPromises()

    socket.emit(
      makeStudentOfflineEvent({ studentId: 'student-1', sessionId: 'session-1', reason: 'LEFT' }),
    )
    await flushPromises()

    expect(harness.current().unsubscribeCalls).toEqual(['session-1'])
    expect(tileOf(wrapper, 'student-1').find('[data-testid="tile-badge"]').text()).toContain(
      '已离开',
    )
  })

  it('§47：实时状态没连上时顶部明确提示"可能不是最新"，并给重新加载', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()
    expect(wrapper.find('[data-testid="monitor-realtime-status"]').exists()).toBe(false)

    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore(pinia)
    realtime.start()
    await flushPromises()

    const banner = wrapper.find('[data-testid="monitor-realtime-status"]')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toContain('正在连接实时状态')

    socket.open()
    await flushPromises()
    expect(wrapper.find('[data-testid="monitor-realtime-status"]').exists()).toBe(false)

    socket.close()
    await flushPromises()
    const degraded = wrapper.find('[data-testid="monitor-realtime-status"]')
    expect(degraded.text()).toContain('重连')
    expect(degraded.text()).toContain('可能不是最新')
  })

  it('§47：WS 断开时兜底轮询收紧到 20 秒（这段窗口里快照是唯一来源）', async () => {
    vi.useFakeTimers()
    installFakeMonitorRoom()
    installFakeVisibility()
    await mountView()
    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore(pinia)
    realtime.start()
    socket.open()
    await vi.advanceTimersByTimeAsync(0)
    const callsAfterOpen = getMonitorMock.mock.calls.length

    socket.close()
    await vi.advanceTimersByTimeAsync(20_000)

    expect(getMonitorMock.mock.calls.length).toBeGreaterThan(callsAfterOpen)
  })

  it('§49：ROOM_CLOSED 时页面说明"课堂已经结束"，并且不再显示媒体错误', async () => {
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()

    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore(pinia)
    realtime.start()
    socket.open()
    await flushPromises()

    socket.emit(makeRoomClosedEvent({ classroomId: 'room-1' }))
    await flushPromises()

    const notice = wrapper.find('[data-testid="monitor-classroom-closed"]')
    expect(notice.exists()).toBe(true)
    expect(notice.text()).toContain('本课堂已经结束')
    // 关课导致的媒体释放是正常结果，不该被渲染成"无法看到学生画面"的故障。
    expect(wrapper.find('[data-testid="monitor-media-error"]').exists()).toBe(false)
  })

  it('§47：自动重连放弃后给出「重试实时连接」（服务端恢复后不必刷新整页）', async () => {
    vi.useFakeTimers()
    installFakeMonitorRoom()
    installFakeVisibility()
    const { wrapper } = await mountView()
    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore(pinia)
    realtime.start()

    for (let attempt = 0; attempt < 3; attempt += 1) {
      socket.current().serverClose()
      await vi.advanceTimersByTimeAsync(30_000)
    }
    expect(realtime.authFailed).toBe(true)
    await vi.advanceTimersByTimeAsync(0)

    const retry = wrapper.find('[data-testid="monitor-realtime-retry"]')
    expect(retry.exists()).toBe(true)
    const socketsBefore = socket.sockets.length

    await retry.trigger('click')

    expect(socket.sockets.length).toBe(socketsBefore + 1)
    expect(realtime.authFailed).toBe(false)
  })
})
