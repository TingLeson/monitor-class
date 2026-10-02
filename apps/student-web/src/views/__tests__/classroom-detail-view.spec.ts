import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenStudentClassroom, makeStudentClassroom } from '../../__tests__/fixtures'
import {
  makeFakeCapture,
  makeMediaDevicesStub,
  type MediaDevicesStub,
} from '../../__tests__/screen-fixtures'
import { ScreenGateError } from '../../lib/screen-capture'
import { routes } from '../../router'
import { useClassroomsStore } from '../../stores/classrooms'
import { useScreenShareStore } from '../../stores/screen-share'
import ClassroomDetailView from '../ClassroomDetailView.vue'

/**
 * PreJoin 页测试（§15 Step 2 / §16 / §17 / §22 / §56 / §57 / §65 Case 6/7/8）。
 *
 * 这一页最重要的断言有几条是"反面"的：
 *
 * 1. §57 的隐私告知必须是**原文**——学生正是靠它才知道老师会看到整块屏幕上的
 *    一切，改一个字都是对这条告知的削弱；
 * 2. 挂载时**绝不**请求屏幕（§57：不要偷偷请求）；
 * 3. 页面上**没有**自己的画面预览（§56：不要 screen in screen）；
 * 4. Gate 失败时就地说明原因、**不跳转**——跳走或伪装成功都会在老师那端留下一个
 *    没有屏幕却"在线"的学生。
 *
 * `requestEntireScreen` 在这里被替换成受控假实现（真实的 Gate 判定由
 * `src/lib/__tests__/screen-capture.spec.ts` 覆盖），但 `checkScreenCaptureSupport`
 * 是**真的**：能力自检必须端到端地真的去读 `navigator.mediaDevices`，
 * 否则"能力不足时禁用主按钮"这条断言就没有意义。
 */
const { getClassroomMock, requestEntireScreenMock, releaseScreenCaptureMock } = vi.hoisted(() => ({
  getClassroomMock: vi.fn(),
  requestEntireScreenMock: vi.fn(),
  releaseScreenCaptureMock: vi.fn(),
}))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: vi.fn(),
  getClassroom: getClassroomMock,
}))

vi.mock('../../lib/screen-capture.ts', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/screen-capture.ts')>()
  return {
    ...actual,
    requestEntireScreen: requestEntireScreenMock,
    releaseScreenCapture: releaseScreenCaptureMock,
  }
})

/** §57 原文（任务书与 docs/frontend/student.md §3.1 完全一致）。 */
const PRIVACY_NOTICE =
  '进入课堂后，老师能够看到当前共享显示器上的内容，包括你在其他应用程序和浏览器中打开的内容。' +
  '请关闭与课堂无关的隐私信息后再继续。'

/** §16 / §17 原文（无法确认共享面时必须出现的那句话）。 */
const UNVERIFIABLE_NOTICE =
  '当前浏览器无法确认你是否共享了完整显示器。请使用系统支持的最新版 Chrome 或 Edge。'

const OPEN = makeOpenStudentClassroom({ id: 'room-open', name: 'C++ 算法训练' })
const CLOSED = makeStudentClassroom({ id: 'room-closed', name: '数据结构练习' })

/** 在真实 URL 下挂载 PreJoin 页：视图通过 useRoute().params.id 取课堂 id。 */
async function mountView(id: string) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(`/student/classrooms/${id}`)
  await router.isReady()
  const wrapper = mount(ClassroomDetailView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

/** 装一个"能力自检通过"的假浏览器，并返回它的替身（含调用记录）。 */
function installCapableBrowser(): MediaDevicesStub {
  const devices = makeMediaDevicesStub('monitor')
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    writable: true,
    value: devices,
  })
  return devices
}

describe('PreJoin 页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    getClassroomMock.mockReset().mockResolvedValue(OPEN)
    requestEntireScreenMock.mockReset()
    // releaseScreenCapture 是**真的**会被视图/ store 调用的清理入口，替身里让它
    // 真的去停轨道，这样"卸载后轨道已结束"这类断言才有意义。
    releaseScreenCaptureMock.mockReset().mockImplementation((capture: { stop(): void } | null) => {
      capture?.stop()
    })
  })

  it('顶部显示课堂名、说明副标题、老师与状态', async () => {
    const { wrapper } = await mountView('room-open')

    expect(wrapper.find('[data-testid="classroom-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="classroom-description"]').text()).toBe('第三章 动态规划')
    expect(wrapper.find('[data-testid="classroom-teacher"]').text()).toBe('李老师')
    expect(wrapper.find('[data-testid="classroom-status"]').text()).toContain('已开启')
    expect(getClassroomMock).toHaveBeenCalledWith('room-open')
  })

  it('没有说明时不渲染空的副标题行', async () => {
    getClassroomMock.mockResolvedValue(makeOpenStudentClassroom({ description: null }))
    const { wrapper } = await mountView('room-open')

    expect(wrapper.find('[data-testid="classroom-description"]').exists()).toBe(false)
  })

  it('明确列出「必须：整个显示器」与「可选：摄像头 / 麦克风」', async () => {
    const { wrapper } = await mountView('room-open')

    const required = wrapper.find('[data-testid="prejoin-required"]')
    expect(required.text()).toContain('必须')
    expect(required.text()).toContain('整个显示器')
    // 只说"共享屏幕"不够：学生必须知道那是整块显示器，而不是某个窗口。
    expect(required.text()).toContain('整块屏幕')

    const optional = wrapper.find('[data-testid="prejoin-optional"]')
    expect(optional.text()).toContain('可选')
    expect(optional.text()).toContain('摄像头')
    expect(optional.text()).toContain('麦克风')
    // 可选必须写清楚"不影响进入课堂"，否则学生会以为不授权就进不去（§24/§25）。
    expect(optional.text()).toContain('不影响进入课堂')

    // 可选设备搞错分组（混进"必须"）会让整页的告知失真。
    expect(required.text()).not.toContain('摄像头')
    expect(required.text()).not.toContain('麦克风')
  })

  it('§57 隐私告知原文出现（不得改写、不得省略）', async () => {
    const { wrapper } = await mountView('room-open')

    const notice = wrapper.find('[data-testid="privacy-notice"]')
    expect(notice.exists()).toBe(true)
    expect(notice.text().trim()).toBe(PRIVACY_NOTICE)
  })

  it('挂载时绝不请求屏幕（§57：不要偷偷请求）', async () => {
    const devices = installCapableBrowser()
    const { wrapper } = await mountView('room-open')

    expect(devices.calls).toHaveLength(0)
    expect(requestEntireScreenMock).not.toHaveBeenCalled()
    // 也不能靠"先渲染一个 video 再去要流"——那本身就是一次捕获意图（§56）。
    expect(wrapper.find('video').exists()).toBe(false)
  })

  it('OPEN + 能力具备：主按钮可用，点击后才请求整屏', async () => {
    const devices = installCapableBrowser()
    requestEntireScreenMock.mockResolvedValue(makeFakeCapture())
    const { wrapper } = await mountView('room-open')

    const button = wrapper.find('[data-testid="enter-classroom"]')
    expect(button.text()).toBe('共享整个屏幕并进入课堂')
    expect(button.attributes('disabled')).toBeUndefined()
    expect(devices.calls).toHaveLength(0)

    await button.trigger('click')
    await flushPromises()

    expect(requestEntireScreenMock).toHaveBeenCalledTimes(1)
  })

  it('Gate 通过：显示「正在共享整个屏幕」+ 诊断值 + Phase 6 说明，且没有自身预览', async () => {
    installCapableBrowser()
    requestEntireScreenMock.mockResolvedValue(makeFakeCapture())
    const { wrapper } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="screen-sharing-status"]').text()).toBe('🖥 正在共享整个屏幕')
    // displaySurface 的原始值是 Gate 的唯一判据，展示出来才有诊断价值。
    expect(wrapper.find('[data-testid="screen-surface"]').text()).toBe('monitor')
    expect(wrapper.find('[data-testid="phase6-notice"]').text()).toContain(
      '进入课堂将在 Phase 6 接入',
    )
    expect(wrapper.find('[data-testid="phase6-notice"]').text()).toContain('LiveKit')
    // 共享中不再显示主按钮（避免第二次请求 / 重复授权弹窗）。
    expect(wrapper.find('[data-testid="enter-classroom"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="stop-screen-share"]').exists()).toBe(true)
    // §56：绝不显示自己的屏幕预览。
    expect(wrapper.find('video').exists()).toBe(false)
  })

  it('选了窗口（SCREEN_NOT_MONITOR）：就地展示原因、不跳转、可重试（§65 Case 7）', async () => {
    installCapableBrowser()
    requestEntireScreenMock.mockRejectedValue(
      new ScreenGateError('SCREEN_NOT_MONITOR', { surface: 'window' }),
    )
    const { wrapper, router } = await mountView('room-open')
    const before = router.currentRoute.value.fullPath

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()

    const error = wrapper.find('[data-testid="screen-gate-error"]')
    expect(error.exists()).toBe(true)
    expect(error.text()).toContain('应用窗口')
    expect(error.text()).toContain('「整个屏幕」')
    // 明确说出"你选的是什么"，并且要求重选，而不是重复同一句话。
    expect(error.text()).toContain('不要选择窗口或浏览器标签页')
    // 错误码本身是给程序看的，不该出现在学生界面上（§58）。
    expect(error.text()).not.toContain('SCREEN_NOT_MONITOR')

    // 不跳转：Gate 失败必须留在 PreJoin 页，并且按钮变成"重新共享"。
    expect(router.currentRoute.value.fullPath).toBe(before)
    expect(wrapper.find('[data-testid="enter-classroom"]').text()).toBe('重新共享整个屏幕')
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(false)
  })

  it('用户取消授权（NotAllowedError）：展示「未获得屏幕共享权限」并可重试', async () => {
    installCapableBrowser()
    requestEntireScreenMock.mockRejectedValue(
      new ScreenGateError('SCREEN_PERMISSION_DENIED', { causeName: 'NotAllowedError' }),
    )
    const { wrapper } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()

    const error = wrapper.find('[data-testid="screen-gate-error"]')
    expect(error.text()).toContain('未获得屏幕共享权限')
    expect(error.text()).toContain('必须共享整个屏幕')
    expect(wrapper.find('[data-testid="enter-classroom"]').attributes('disabled')).toBeUndefined()
  })

  it('NotReadableError（系统未授予屏幕录制权限）：文案指向系统设置，不说"你选错了窗口"', async () => {
    installCapableBrowser()
    requestEntireScreenMock.mockRejectedValue(
      new ScreenGateError('SCREEN_PERMISSION_DENIED', { causeName: 'NotReadableError' }),
    )
    const { wrapper } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()

    const error = wrapper.find('[data-testid="screen-gate-error"]')
    expect(error.text()).toContain('无法开始屏幕捕获')
    expect(error.text()).toContain('屏幕录制')
    // 报成"共享窗口无法进入课堂"会让学生反复重选整个屏幕却永远失败。
    expect(error.text()).not.toContain('不要选择窗口')
  })

  it('displaySurface 缺失：展示 §16/§17 原文那段"无法确认"', async () => {
    installCapableBrowser()
    requestEntireScreenMock.mockRejectedValue(
      new ScreenGateError('SCREEN_API_UNSUPPORTED', {
        surface: 'unknown',
        causeName: 'displaySurface',
      }),
    )
    const { wrapper } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()

    const error = wrapper.find('[data-testid="screen-gate-error"]')
    expect(error.text()).toContain('无法确认')
    expect(error.text()).toContain(UNVERIFIABLE_NOTICE)
  })

  it('共享被结束：显示「已停止屏幕共享 / 当前课堂要求持续共享整个屏幕」+ 重新共享', async () => {
    installCapableBrowser()
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const { wrapper } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)

    capture.emitEnded()
    await flushPromises()

    const lost = wrapper.find('[data-testid="screen-lost"]')
    expect(lost.exists()).toBe(true)
    expect(lost.text()).toContain('已停止屏幕共享')
    expect(lost.text()).toContain('当前课堂要求持续共享整个屏幕')

    const button = wrapper.find('[data-testid="enter-classroom"]')
    expect(button.text()).toBe('重新共享整个屏幕')
    expect(button.attributes('disabled')).toBeUndefined()
    // 断了就是断了：不能还挂着"正在共享"。
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(false)
    expect(releaseScreenCaptureMock).toHaveBeenCalled()
    expect(capture.track.readyState).toBe('ended')

    // §65 Case 11：重新共享能恢复"正在共享"状态。
    requestEntireScreenMock.mockResolvedValue(makeFakeCapture())
    await button.trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="screen-lost"]').exists()).toBe(false)
  })

  it('重复点击只触发一次捕获（请求飞行中按钮禁用 + store 重入保护）', async () => {
    installCapableBrowser()
    let release: (capture: ReturnType<typeof makeFakeCapture>) => void = () => undefined
    requestEntireScreenMock.mockReturnValue(
      new Promise((resolve) => {
        release = resolve
      }),
    )
    const { wrapper } = await mountView('room-open')

    const button = wrapper.find('[data-testid="enter-classroom"]')
    await button.trigger('click')
    await button.trigger('click')
    await button.trigger('click')
    await flushPromises()

    expect(requestEntireScreenMock).toHaveBeenCalledTimes(1)
    // 飞行中禁用按钮：学生不会因为"没反应"而一直点（每次点都可能弹一个新窗口）。
    expect(wrapper.find('[data-testid="enter-classroom"]').attributes('disabled')).toBeDefined()

    release(makeFakeCapture())
    await flushPromises()

    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)
    expect(requestEntireScreenMock).toHaveBeenCalledTimes(1)
  })

  it('能力不足：主按钮禁用 + 阻断式说明 + 点击也不会请求（§17）', async () => {
    // 不安装 mediaDevices：happy-dom 默认就没有，正是 §17 检测的第一种情况。
    const { wrapper } = await mountView('room-open')

    const button = wrapper.find('[data-testid="enter-classroom"]')
    expect(button.text()).toBe('共享整个屏幕并进入课堂')
    expect(button.attributes('disabled')).toBeDefined()

    const notice = wrapper.find('[data-testid="screen-unsupported-notice"]')
    expect(notice.exists()).toBe(true)
    expect(notice.text()).toContain(UNVERIFIABLE_NOTICE)

    await button.trigger('click')
    await flushPromises()

    expect(requestEntireScreenMock).not.toHaveBeenCalled()
    // 绝不静默降级成窗口共享：能力不足就是进不去。
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="enter-disabled-hint"]').exists()).toBe(true)
  })

  it('CLOSED：保持「老师尚未开启本课堂」，主按钮禁用且不请求', async () => {
    installCapableBrowser()
    getClassroomMock.mockResolvedValue(CLOSED)
    const { wrapper } = await mountView('room-closed')

    expect(wrapper.find('[data-testid="classroom-status"]').text()).toContain('未开启')
    expect(wrapper.find('[data-testid="classroom-closed-notice"]').text()).toContain(
      '老师尚未开启本课堂',
    )
    expect(wrapper.find('[data-testid="enter-classroom"]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="enter-disabled-hint"]').text()).toContain(
      '老师尚未开启本课堂',
    )
    expect(wrapper.find('[data-testid="back-to-classrooms"]').attributes('href')).toBe(
      '/student/classrooms',
    )

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()

    expect(requestEntireScreenMock).not.toHaveBeenCalled()
  })

  it('离开页面时释放屏幕轨道（§65 Case 14：退出课堂即停止捕获）', async () => {
    installCapableBrowser()
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const { wrapper } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)

    wrapper.unmount()

    expect(releaseScreenCaptureMock).toHaveBeenCalledWith(capture)
    expect(capture.track.readyState).toBe('ended')
    expect(useScreenShareStore().status).toBe('idle')
  })

  it('切换课堂时重置共享状态（不会把上一个课堂的共享带到新课堂）', async () => {
    installCapableBrowser()
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    getClassroomMock.mockImplementation((id: string) =>
      Promise.resolve(
        id === 'room-open' ? OPEN : makeOpenStudentClassroom({ id: 'room-b', name: 'B 课堂' }),
      ),
    )
    const { wrapper, router } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()
    expect(useScreenShareStore().status).toBe('sharing')

    await router.push('/student/classrooms/room-b')
    await flushPromises()

    expect(useScreenShareStore().status).toBe('idle')
    expect(releaseScreenCaptureMock).toHaveBeenCalledWith(capture)
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(false)
  })

  it('共享中点击「停止共享」：释放轨道并回到可重新共享的状态', async () => {
    installCapableBrowser()
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const { wrapper } = await mountView('room-open')

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)

    await wrapper.find('[data-testid="stop-screen-share"]').trigger('click')
    await flushPromises()

    expect(capture.track.readyState).toBe('ended')
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(false)
    // 主动停止是学生自己的决定，不该显示成 §22 的"意外断开"。
    expect(wrapper.find('[data-testid="screen-lost"]').exists()).toBe(false)
    // 停止后必须能重新开始，否则这一页就成了死路。
    expect(wrapper.find('[data-testid="enter-classroom"]').text()).toBe('共享整个屏幕并进入课堂')
    expect(wrapper.find('[data-testid="enter-classroom"]').attributes('disabled')).toBeUndefined()
  })

  it('所有状态下页面上都没有任何 video 元素（§56 不做自身预览）', async () => {
    installCapableBrowser()
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const { wrapper } = await mountView('room-open')

    // idle
    expect(wrapper.findAll('video')).toHaveLength(0)

    await wrapper.find('[data-testid="enter-classroom"]').trigger('click')
    await flushPromises()
    // sharing：这正是最容易"顺手加个预览"的时刻（screen in screen）。
    expect(wrapper.findAll('video')).toHaveLength(0)
    expect(wrapper.find('[data-testid="screen-sharing-status"]').exists()).toBe(true)

    capture.emitEnded()
    await flushPromises()
    // lost
    expect(wrapper.findAll('video')).toHaveLength(0)
  })

  it('404 STUDENT_NOT_ASSIGNED：说明不在名单里 + 回列表入口，不给"重试"', async () => {
    getClassroomMock.mockRejectedValue(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 }))
    const { wrapper } = await mountView('room-x')

    const state = wrapper.find('[data-testid="classroom-not-assigned"]')
    expect(state.text()).toContain('你不在这个课堂的名单里')
    expect(state.text()).toContain('请联系老师确认')
    expect(wrapper.find('[data-testid="back-to-classrooms"]').attributes('href')).toBe(
      '/student/classrooms',
    )
    // 停在这一页反复重试是没有意义的：授权不会因为重试而改变。
    expect(wrapper.find('[data-testid="classroom-detail-error"]').exists()).toBe(false)
    expect(state.find('button').exists()).toBe(false)
    // 也不能显示成"课堂不存在"——那会让未被授权的学生以为自己猜错了 id（§63）。
    expect(wrapper.text()).not.toContain('课堂不存在')
  })

  it('网络失败：显示错误文案与重试，重试成功后渲染课堂', async () => {
    getClassroomMock.mockRejectedValueOnce(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const { wrapper } = await mountView('room-open')

    const error = wrapper.find('[data-testid="classroom-detail-error"]')
    expect(error.text()).toContain('网络连接失败')

    getClassroomMock.mockResolvedValue(OPEN)
    await error.find('button').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="classroom-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="classroom-detail-error"]').exists()).toBe(false)
  })

  it('切换 :id 时先清空：新课堂的数据到达前，页面上不会留着上一个课堂的名字', async () => {
    const A = makeOpenStudentClassroom({ id: 'room-a', name: 'A 课堂' })
    const B = makeStudentClassroom({ id: 'room-b', name: 'B 课堂', status: 'OPEN' })
    // B 的响应永远不来：要验证的正是"在 B 的数据到达之前，页面上已经没有 A 了"。
    getClassroomMock.mockImplementation((id: string) => {
      if (id === A.id) return Promise.resolve(A)
      if (id === B.id) return new Promise(() => undefined)
      return Promise.reject(new Error(`未预期的课堂 id：${id}`))
    })
    const { wrapper, router } = await mountView('room-a')
    expect(wrapper.find('[data-testid="classroom-name"]').text()).toBe('A 课堂')

    await router.push('/student/classrooms/room-b')
    await flushPromises()

    expect(wrapper.text()).not.toContain('A 课堂')
    expect(wrapper.text()).toContain('正在加载课堂')
  })

  it('从已授权的课堂切到不在名单里的课堂：显示未授权说明，不残留上一个课堂', async () => {
    const A = makeOpenStudentClassroom({ id: 'room-a', name: 'A 课堂' })
    getClassroomMock.mockImplementation((id: string) =>
      id === 'room-a'
        ? Promise.resolve(A)
        : Promise.reject(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 })),
    )
    const { wrapper, router } = await mountView('room-a')

    await router.push('/student/classrooms/room-x')
    await flushPromises()

    expect(wrapper.text()).not.toContain('A 课堂')
    expect(wrapper.find('[data-testid="classroom-not-assigned"]').exists()).toBe(true)
  })

  it('离开页面时清空 store 里的详情（下次进入不会闪现上一个课堂）', async () => {
    const { wrapper } = await mountView('room-open')
    const store = useClassroomsStore()
    expect(store.current?.id).toBe('room-open')

    wrapper.unmount()

    expect(store.current).toBeNull()
    expect(store.currentError).toBeNull()
  })

  it('数据到达前不渲染空白页，而是显示加载占位', async () => {
    getClassroomMock.mockReturnValue(new Promise(() => undefined))
    const { wrapper } = await mountView('room-open')

    expect(wrapper.text()).toContain('正在加载课堂')
    expect(wrapper.find('[data-testid="classroom-name"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="classroom-not-assigned"]').exists()).toBe(false)
  })
})
