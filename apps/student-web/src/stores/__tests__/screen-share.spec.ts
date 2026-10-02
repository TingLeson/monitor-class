import { createPinia, setActivePinia } from 'pinia'
import { isProxy, nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeFakeCapture } from '../../__tests__/screen-fixtures'
import { ScreenGateError, type ScreenCaptureSupport } from '../../lib/screen-capture'
import { useScreenShareStore } from '../screen-share'

/**
 * 屏幕共享状态机测试（§16 / §18 / §22 / §57）。
 *
 * 与 `screen-capture.spec.ts` 的分工：那边测"浏览器说了什么、Gate 怎么判"，
 * 这边测"判定结果如何变成学生看到的状态"。因此这里把 `requestEntireScreen`
 * 整个替换成受控的假实现——真实的 Gate 逻辑已经在上一份测试里逐条覆盖过。
 *
 * 两条最关键的断言是**反面**的：
 * 1. `sharing` 只在 Gate 通过后出现（绝不做乐观更新，§18）；
 * 2. 任何退出路径都真的把轨道放掉了（§22 / §65 Case 14）。
 */
const {
  checkScreenCaptureSupportMock,
  requestEntireScreenMock,
  releaseScreenCaptureMock,
  describeScreenCaptureMock,
} = vi.hoisted(() => ({
  checkScreenCaptureSupportMock: vi.fn(),
  requestEntireScreenMock: vi.fn(),
  releaseScreenCaptureMock: vi.fn(),
  describeScreenCaptureMock: vi.fn(),
}))

vi.mock('../../lib/screen-capture.ts', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/screen-capture.ts')>()
  return {
    ...actual,
    checkScreenCaptureSupport: checkScreenCaptureSupportMock,
    requestEntireScreen: requestEntireScreenMock,
    releaseScreenCapture: releaseScreenCaptureMock,
    describeScreenCapture: describeScreenCaptureMock,
  }
})

const SUPPORTED: ScreenCaptureSupport = { supported: true }

describe('useScreenShareStore（§16 / §18 / §22 状态机）', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    checkScreenCaptureSupportMock.mockReset().mockReturnValue(SUPPORTED)
    requestEntireScreenMock.mockReset()
    releaseScreenCaptureMock.mockReset()
    describeScreenCaptureMock.mockReset().mockReturnValue({
      displaySurface: 'monitor',
      rawDisplaySurface: 'monitor',
      surfaceActive: null,
    })
  })

  it('初始状态是 idle，且没有任何捕获', () => {
    const store = useScreenShareStore()

    expect(store.status).toBe('idle')
    expect(store.capture).toBeNull()
    expect(store.diagnostics).toBeNull()
    expect(store.failure).toBeNull()
    expect(store.unsupportedReason).toBeNull()
  })

  it('start()：requesting → sharing，并保存诊断信息', async () => {
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const store = useScreenShareStore()

    const pending = store.start()
    // 请求飞行期间必须是 requesting：界面靠它显示"请在浏览器里选择整个屏幕"。
    expect(store.status).toBe('requesting')

    await pending

    expect(store.status).toBe('sharing')
    expect(store.isSharing).toBe(true)
    expect(store.capture).toBe(capture)
    expect(store.diagnostics).toEqual({
      displaySurface: 'monitor',
      rawDisplaySurface: 'monitor',
      surfaceActive: null,
    })
  })

  it('Gate 拒绝（选了窗口）→ error，界面可读到错误码与共享面', async () => {
    requestEntireScreenMock.mockRejectedValue(
      new ScreenGateError('SCREEN_NOT_MONITOR', { surface: 'window' }),
    )
    const store = useScreenShareStore()

    await store.start()

    expect(store.status).toBe('error')
    expect(store.hasFailed).toBe(true)
    expect(store.failure).toEqual({
      code: 'SCREEN_NOT_MONITOR',
      surface: 'window',
      causeName: null,
    })
    expect(store.capture).toBeNull()
  })

  it('NotReadableError → 保留 causeName，文案层才能说成"系统没给屏幕录制权限"', async () => {
    requestEntireScreenMock.mockRejectedValue(
      new ScreenGateError('SCREEN_PERMISSION_DENIED', { causeName: 'NotReadableError' }),
    )
    const store = useScreenShareStore()

    await store.start()

    expect(store.failure).toEqual({
      code: 'SCREEN_PERMISSION_DENIED',
      surface: null,
      causeName: 'NotReadableError',
    })
  })

  it('能力不足：**不调用** getDisplayMedia，直接进入 error（§17）', async () => {
    checkScreenCaptureSupportMock.mockReturnValue({
      supported: false,
      reason: 'no-getDisplayMedia',
    })
    const store = useScreenShareStore()

    await store.start()

    expect(store.status).toBe('error')
    expect(store.unsupportedReason).toBe('no-getDisplayMedia')
    expect(requestEntireScreenMock).not.toHaveBeenCalled()
  })

  it('重复点击只触发一次捕获（§57：不要在学生以为"没反应"时弹第二个授权窗口）', async () => {
    let release: (capture: ReturnType<typeof makeFakeCapture>) => void = () => undefined
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockReturnValue(
      new Promise((resolve) => {
        release = resolve
      }),
    )
    const store = useScreenShareStore()

    const first = store.start()
    const second = store.start()
    const third = store.start()

    expect(requestEntireScreenMock).toHaveBeenCalledTimes(1)

    release(capture)
    await Promise.all([first, second, third])

    expect(store.status).toBe('sharing')
    expect(requestEntireScreenMock).toHaveBeenCalledTimes(1)
  })

  it('已经在共享中时再 start() 不会重新请求', async () => {
    requestEntireScreenMock.mockResolvedValue(makeFakeCapture())
    const store = useScreenShareStore()
    await store.start()

    await store.start()

    expect(requestEntireScreenMock).toHaveBeenCalledTimes(1)
    expect(store.status).toBe('sharing')
  })

  it('track ended → lost，轨道被释放，界面能拿到 §22 的文案依据', async () => {
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const store = useScreenShareStore()
    await store.start()

    capture.emitEnded()

    expect(store.status).toBe('lost')
    expect(store.isLost).toBe(true)
    expect(store.capture).toBeNull()
    expect(capture.track.readyState).toBe('ended')
    expect(store.failure).toEqual({
      code: 'SCREEN_TRACK_ENDED',
      surface: 'monitor',
      causeName: 'ended',
    })
  })

  it('lost 之后再 start() 可以恢复共享（§65 Case 11）', async () => {
    const first = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValueOnce(first)
    const store = useScreenShareStore()
    await store.start()
    first.emitEnded()
    expect(store.status).toBe('lost')

    const second = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValueOnce(second)
    await store.start()

    expect(store.status).toBe('sharing')
    expect(requestEntireScreenMock).toHaveBeenCalledTimes(2)
  })

  it('stop()：回到 idle 并释放轨道（主动停止不该显示成"意外断开"）', async () => {
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const store = useScreenShareStore()
    await store.start()

    store.stop()

    expect(store.status).toBe('idle')
    expect(store.capture).toBeNull()
    /**
     * 这里断言的是"store 把释放**交给了** releaseScreenCapture"，而不是轨道真的
     * 变成 ended：本文件把整个 screen-capture 模块替换成了替身（真实的 stop 语义
     * 由 `screen-capture.spec.ts` 覆盖）。store 的职责是"有没有调用释放"。
     */
    expect(releaseScreenCaptureMock).toHaveBeenCalledWith(capture)

    // 再 stop 一次也必须安全（视图卸载 + 显式停止会各调一次）。
    store.stop()
    expect(store.status).toBe('idle')
  })

  it('reset()：与 stop() 等价，且清掉失败信息（切换课堂时用）', async () => {
    requestEntireScreenMock.mockRejectedValue(new ScreenGateError('SCREEN_NOT_MONITOR'))
    const store = useScreenShareStore()
    await store.start()
    expect(store.failure).not.toBeNull()

    store.reset()

    expect(store.status).toBe('idle')
    expect(store.failure).toBeNull()
    expect(store.unsupportedReason).toBeNull()
  })

  it('请求飞行中被 stop()：迟到的轨道会被立刻释放，不会变成"无人能停"的 live 轨道', async () => {
    let release: (capture: ReturnType<typeof makeFakeCapture>) => void = () => undefined
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockReturnValue(
      new Promise((resolve) => {
        release = resolve
      }),
    )
    const store = useScreenShareStore()
    const pending = store.start()

    store.stop()
    release(capture)
    await pending

    expect(store.status).toBe('idle')
    expect(store.capture).toBeNull()
    // 这条轨道没有主人了：必须被释放，否则学生被共享着却没有任何界面能停它。
    expect(releaseScreenCaptureMock).toHaveBeenCalledWith(capture)
  })

  it('保存的捕获对象**不是**响应式代理（markRaw：防止原生调用 Illegal invocation）', async () => {
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const store = useScreenShareStore()
    await store.start()

    expect(isProxy(store.capture)).toBe(false)
    // 身份也必须原样保留：被代理过的 track 与流里的 track 不再全等。
    expect(store.capture).toBe(capture)
  })

  it('状态变化会通知视图（sharing → lost 时界面必须重渲染）', async () => {
    const capture = makeFakeCapture()
    requestEntireScreenMock.mockResolvedValue(capture)
    const store = useScreenShareStore()
    await store.start()

    const seen: string[] = []
    const stopWatching = store.$subscribe(() => seen.push(store.status), { flush: 'sync' })

    capture.emitEnded()
    await nextTick()

    stopWatching()
    expect(seen).toContain('lost')
  })
})
