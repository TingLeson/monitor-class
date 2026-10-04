import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  checkScreenCaptureSupport,
  releaseScreenCapture,
  requestEntireScreen,
  SCREEN_CAPTURE_CONSTRAINTS,
  ScreenGateError,
  surfaceLabel,
  type ScreenCapture,
} from '../screen-capture'
import { describeScreenGateFailure, SCREEN_UNVERIFIABLE_NOTICE } from '../screen-capture-messages'
import {
  installMediaDevices,
  makeFakeStream,
  makeFakeTrack,
  makeMediaDevicesStub,
  type MediaDevicesStub,
} from '../../__tests__/screen-fixtures'

/**
 * Screen Gate 单测（§16 / §17 / §22 / §64 "Screen Gate"；§65 Case 6/7/8）。
 *
 * 这里覆盖的是本 Phase 的全部业务判定。最重要的不是"monitor 能过"
 * （那是 Case 8，最容易写），而是**每一条拒绝路径都真的调用了 `track.stop()`**：
 * 只拒绝不停轨道，学生会得到一个"进不去课堂、但浏览器一直在共享屏幕"的状态——
 * 既没上课，又一直在被采集。这是本项目最不能出现的一种失败。
 *
 * 另一个重点是 §17 的能力自检：三种缺失都必须**在请求之前**拒绝，
 * 断言里显式检查 `getDisplayMedia` 没被调用（而不是只检查抛了什么错）。
 */
describe('checkScreenCaptureSupport（§17 能力自检）', () => {
  it('三项都具备时通过', () => {
    installMediaDevices(makeMediaDevicesStub())
    expect(checkScreenCaptureSupport()).toEqual({ supported: true })
  })

  it('没有 navigator.mediaDevices → no-mediaDevices', () => {
    // happy-dom 默认就没有 mediaDevices，恰好就是 §17 要检测的第一种情况。
    expect(checkScreenCaptureSupport()).toEqual({
      supported: false,
      reason: 'no-mediaDevices',
    })
  })

  it('mediaDevices 存在但没有 getDisplayMedia → no-getDisplayMedia', () => {
    installMediaDevices({})
    expect(checkScreenCaptureSupport()).toEqual({
      supported: false,
      reason: 'no-getDisplayMedia',
    })
  })

  it('getDisplayMedia 存在但 MediaStreamTrack 没有 getSettings → no-getSettings', () => {
    installMediaDevices(makeMediaDevicesStub())
    const original = Reflect.get(globalThis, 'MediaStreamTrack')
    try {
      /**
       * 模拟"这个浏览器没有 getSettings"：把全局 MediaStreamTrack 换成一个
       * 没有该原型方法的构造器。**必须**通过 globalThis 替换而不是直接改
       * `MediaStreamTrack.prototype`——happy-dom 的 MediaStreamTrack 是个空壳
       * 类（原型上连 getSettings 都没有，走的是 Proxy 兜底），删原型方法根本删不掉。
       */
      Object.defineProperty(globalThis, 'MediaStreamTrack', {
        configurable: true,
        writable: true,
        value: function MediaStreamTrackWithoutSettings() {},
      })
      expect(checkScreenCaptureSupport()).toEqual({
        supported: false,
        reason: 'no-getSettings',
      })
    } finally {
      Object.defineProperty(globalThis, 'MediaStreamTrack', {
        configurable: true,
        writable: true,
        value: original,
      })
    }
  })

  it('全局连 MediaStreamTrack 都没有时也不抛异常，只报告不支持', () => {
    installMediaDevices(makeMediaDevicesStub())
    const original = Reflect.get(globalThis, 'MediaStreamTrack')
    try {
      Reflect.deleteProperty(globalThis, 'MediaStreamTrack')
      // 自检函数本身把页面搞崩，比"检测不到"糟糕得多。
      expect(checkScreenCaptureSupport()).toEqual({
        supported: false,
        reason: 'no-getSettings',
      })
    } finally {
      Object.defineProperty(globalThis, 'MediaStreamTrack', {
        configurable: true,
        writable: true,
        value: original,
      })
    }
  })

  it('自检本身不产生任何副作用：不请求权限、不调用 getDisplayMedia', () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)

    checkScreenCaptureSupport()

    expect(devices.calls).toHaveLength(0)
  })
})

describe('requestEntireScreen（§16 硬 Gate）', () => {
  let devices: MediaDevicesStub

  beforeEach(() => {
    devices = makeMediaDevicesStub()
    installMediaDevices(devices)
  })

  it('能力不足时不调用 getDisplayMedia，直接抛 SCREEN_API_UNSUPPORTED', async () => {
    const noMediaDevices = makeMediaDevicesStub()
    installMediaDevices({})

    await expect(requestEntireScreen()).rejects.toMatchObject({ code: 'SCREEN_API_UNSUPPORTED' })
    expect(noMediaDevices.calls).toHaveLength(0)
  })

  it('monitor → 通过，且约束表达了"偏好整屏 + 禁止换面"（§16）', async () => {
    const capture = await requestEntireScreen()

    expect(capture.surface).toBe('monitor')
    expect(capture.settings.displaySurface).toBe('monitor')

    const call = devices.calls[0]
    expect(call?.surface).toBe('monitor')
    const constraints = call?.constraints as {
      video?: { displaySurface?: unknown }
      audio?: unknown
      selfBrowserSurface?: unknown
      surfaceSwitching?: unknown
      systemAudio?: unknown
      monitorTypeSurfaces?: unknown
      preferCurrentTab?: unknown
    }
    expect(constraints.video?.displaySurface).toBe('monitor')
    expect(constraints.audio).toBe(false)
    // 不允许共享中换共享面：否则"先用整屏过 Gate，再切换成窗口"就是一条绕过路径。
    expect(constraints.surfaceSwitching).toBe('exclude')
    // 不把自己这个标签页列为可选项（共享一个说明页毫无监督意义）。
    expect(constraints.selfBrowserSurface).toBe('exclude')
    expect(constraints.preferCurrentTab).toBe(false)
    expect(constraints.systemAudio).toBe('exclude')
    // 显式保留"整个屏幕"选项：改成 exclude 会让本 Gate 永远无法通过。
    expect(constraints.monitorTypeSurfaces).toBe('include')

    // 导出的常量必须与真正传下去的对象一致（防止有人在调用点另写一份约束）。
    expect(constraints).toEqual(SCREEN_CAPTURE_CONSTRAINTS)
  })

  it('window → SCREEN_NOT_MONITOR，并且所有 track 都被 stop（§65 Case 7）', async () => {
    devices.surface = 'window'
    const track = makeFakeTrack('window')
    const extra = makeFakeTrack('monitor')
    devices.getDisplayMedia = () => Promise.resolve(makeFakeStream([track, extra]))

    await expect(requestEntireScreen()).rejects.toMatchObject({
      code: 'SCREEN_NOT_MONITOR',
      surface: 'window',
    })
    expect(track.stopCalls).toBe(1)
    // "所有 track"包括同一条流里的其它轨道：留下任何一条，浏览器就仍显示正在共享。
    expect(extra.stopCalls).toBe(1)
  })

  it('browser → SCREEN_NOT_MONITOR，并且 did stop track（§65 Case 6）', async () => {
    devices.surface = 'browser'
    const track = makeFakeTrack('browser')
    devices.getDisplayMedia = () => Promise.resolve(makeFakeStream([track]))

    await expect(requestEntireScreen()).rejects.toMatchObject({
      code: 'SCREEN_NOT_MONITOR',
      surface: 'browser',
    })
    expect(track.stopCalls).toBe(1)
  })

  it('displaySurface 缺失 → 拒绝（SCREEN_API_UNSUPPORTED）且 stop 被调用', async () => {
    const track = makeFakeTrack('missing')
    devices.getDisplayMedia = () => Promise.resolve(makeFakeStream([track]))

    await expect(requestEntireScreen()).rejects.toMatchObject({
      code: 'SCREEN_API_UNSUPPORTED',
      surface: 'unknown',
    })
    expect(track.stopCalls).toBe(1)
  })

  it('displaySurface 为空字符串 → 同样视为"无法确认"并拒绝', async () => {
    const track = makeFakeTrack('empty')
    devices.getDisplayMedia = () => Promise.resolve(makeFakeStream([track]))

    await expect(requestEntireScreen()).rejects.toMatchObject({
      code: 'SCREEN_API_UNSUPPORTED',
    })
    expect(track.stopCalls).toBe(1)
  })

  it('未知的 displaySurface 值 → 一律当非 monitor 拒绝', async () => {
    const track = makeFakeTrack('other')
    devices.getDisplayMedia = () => Promise.resolve(makeFakeStream([track]))

    // 有值但不认识：说明浏览器明确给了个非整屏的共享范围，所以是"选错了"而不是"检测不到"。
    await expect(requestEntireScreen()).rejects.toMatchObject({
      code: 'SCREEN_NOT_MONITOR',
      surface: 'other',
    })
    expect(track.stopCalls).toBe(1)
  })

  it('完全没有视频轨道 → 拒绝并释放剩余轨道', async () => {
    const audio = makeFakeTrack('monitor')
    devices.getDisplayMedia = () => Promise.resolve(makeFakeStream([audio], false))

    await expect(requestEntireScreen()).rejects.toMatchObject({ code: 'SCREEN_TRACK_ENDED' })
    expect(audio.stopCalls).toBe(1)
  })

  it('NotAllowedError（学生取消授权）→ SCREEN_PERMISSION_DENIED', async () => {
    const denied = new Error('Permission denied')
    denied.name = 'NotAllowedError'
    devices.failWith = denied

    await expect(requestEntireScreen()).rejects.toMatchObject({
      code: 'SCREEN_PERMISSION_DENIED',
      causeName: 'NotAllowedError',
    })
  })

  it('NotReadableError（macOS 未授予屏幕录制权限）→ SCREEN_PERMISSION_DENIED，不是"选错共享面"', async () => {
    const notReadable = new Error('Could not start video source')
    notReadable.name = 'NotReadableError'
    devices.failWith = notReadable

    const failure = await requestEntireScreen().catch((cause: unknown) => cause)
    expect(failure).toBeInstanceOf(ScreenGateError)
    expect((failure as ScreenGateError).code).toBe('SCREEN_PERMISSION_DENIED')
    expect((failure as ScreenGateError).causeName).toBe('NotReadableError')
    // 报成 SCREEN_NOT_MONITOR 会让学生反复重选「整个屏幕」却永远失败。
    expect((failure as ScreenGateError).code).not.toBe('SCREEN_NOT_MONITOR')

    // 文案必须指向系统权限，而不是"你选的是窗口"。
    const message = describeScreenGateFailure({
      kind: 'capture',
      code: 'SCREEN_PERMISSION_DENIED',
      causeName: 'NotReadableError',
    })
    expect(message.description).toContain('屏幕录制')
    expect(message.description).not.toContain('整个屏幕」，不要选择窗口')
  })

  it('未知的浏览器错误 → 归到 SCREEN_API_UNSUPPORTED（Gate 的默认方向是拒绝）', async () => {
    devices.failWith = new TypeError('Something went wrong')

    await expect(requestEntireScreen()).rejects.toMatchObject({ code: 'SCREEN_API_UNSUPPORTED' })
  })
})

describe('stop / release / onEnded（§22 客户端一半）', () => {
  let devices: MediaDevicesStub

  beforeEach(() => {
    devices = makeMediaDevicesStub('monitor')
    installMediaDevices(devices)
  })

  it('stop() 幂等：重复调用不会重复停轨道', async () => {
    const capture = await requestEntireScreen()

    capture.stop()
    capture.stop()

    expect((capture.track as unknown as { stopCalls: number }).stopCalls).toBe(1)
    expect(capture.track.readyState).toBe('ended')
  })

  it('releaseScreenCapture 对 null 安全，并停掉轨道', async () => {
    releaseScreenCapture(null)
    releaseScreenCapture(undefined)

    const capture = await requestEntireScreen()
    releaseScreenCapture(capture)

    expect((capture.track as unknown as { stopCalls: number }).stopCalls).toBe(1)
  })

  it('track.onended 形态：派发 ended 后进入结束状态并释放轨道', async () => {
    const capture = await requestEntireScreen()
    const ended = vi.fn()
    capture.onEnded(ended)

    ;(capture.track as unknown as { emitEndedViaProperty: () => void }).emitEndedViaProperty()

    expect(ended).toHaveBeenCalledTimes(1)
    // 释放必须发生在通知之前：否则界面上会有一瞬间"已停止"而轨道仍然 live。
    expect((capture.track as unknown as { stopCalls: number }).stopCalls).toBe(1)
    expect(capture.track.readyState).toBe('ended')
  })

  it("track.addEventListener('ended') 形态：同样能收到并释放", async () => {
    const capture = await requestEntireScreen()
    const ended = vi.fn()
    capture.onEnded(ended)

    ;(capture.track as unknown as { emitEnded: () => void }).emitEnded()

    expect(ended).toHaveBeenCalledTimes(1)
    expect((capture.track as unknown as { stopCalls: number }).stopCalls).toBe(1)
  })

  it('两种形态同时到达也只通知一次（事件回声不会造成第二次状态迁移）', async () => {
    const capture = await requestEntireScreen()
    const ended = vi.fn()
    capture.onEnded(ended)

    const track = capture.track as unknown as {
      emitEnded: () => void
      emitEndedViaProperty: () => void
    }
    track.emitEnded()
    track.emitEndedViaProperty()

    expect(ended).toHaveBeenCalledTimes(1)
  })

  it('取消订阅后不再收到 ended（避免卸载后仍改状态）', async () => {
    const capture = await requestEntireScreen()
    const ended = vi.fn()
    const unsubscribe = capture.onEnded(ended)

    unsubscribe()
    ;(capture.track as unknown as { emitEnded: () => void }).emitEnded()

    expect(ended).not.toHaveBeenCalled()
  })

  it('对已经结束的轨道订阅：立即回调一次（后加入的订阅者也要知道它已经断了）', async () => {
    const capture = await requestEntireScreen()
    capture.stop()

    const ended = vi.fn()
    capture.onEnded(ended)

    expect(ended).toHaveBeenCalledTimes(1)
  })

  it('onEnded 返回的取消函数是可用的一次性句柄（视图卸载时唯一的清理手段）', async () => {
    const capture: ScreenCapture = await requestEntireScreen()
    const ended = vi.fn()
    const unsubscribe = capture.onEnded(ended)

    expect(typeof unsubscribe).toBe('function')
    unsubscribe()
    unsubscribe()

    ;(capture.track as unknown as { emitEnded: () => void }).emitEnded()

    expect(ended).not.toHaveBeenCalled()
  })
})

describe('文案（§16 / §17 / §22 原文）', () => {
  it('能力不足：给出 §17 原文那段"无法确认 + 用最新版 Chrome 或 Edge"', () => {
    const message = describeScreenGateFailure({ kind: 'unsupported', reason: 'no-getDisplayMedia' })

    expect(message.description).toContain(SCREEN_UNVERIFIABLE_NOTICE)
    expect(message.description).toContain('当前浏览器无法确认你是否共享了完整显示器')
    expect(message.description).toContain('请使用系统支持的最新版 Chrome 或 Edge')
  })

  it('http:// 局域网地址（非安全上下文）必须报 insecureContext，而不是"浏览器太旧"', () => {
    // 真实测试暴露的问题：在 http:// + 局域网 IP 下 navigator.mediaDevices 就是
    // undefined，早期实现把它归成 no-mediaDevices，于是界面让学生去"升级浏览器"，
    // 而真正要做的是换成 https://。两者的学生动作完全相反，必须分开。
    const original = Object.getOwnPropertyDescriptor(window, 'isSecureContext')
    Object.defineProperty(window, 'isSecureContext', { value: false, configurable: true })
    try {
      const support = checkScreenCaptureSupport()
      expect(support.supported).toBe(false)
      expect(support.reason).toBe('insecureContext')
      // `reason` 在类型上是可选的（只在 supported === false 时有意义），
      // 所以这里显式收窄一次，而不是用非空断言把类型系统关掉。
      if (support.supported || support.reason === undefined) {
        throw new Error('能力自检应当报 insecureContext')
      }
      const message = describeScreenGateFailure({ kind: 'unsupported', reason: support.reason })
      expect(message.description).toContain('https://')
      expect(message.description).toContain('localhost')
    } finally {
      if (original) Object.defineProperty(window, 'isSecureContext', original)
      else Reflect.deleteProperty(window, 'isSecureContext')
    }
  })

  it('displaySurface 缺失：同样是 §16/§17 原文', () => {
    const message = describeScreenGateFailure({
      kind: 'capture',
      code: 'SCREEN_API_UNSUPPORTED',
      surface: 'unknown',
      causeName: 'displaySurface',
    })

    expect(message.title).toContain('无法确认')
    expect(message.description).toContain(SCREEN_UNVERIFIABLE_NOTICE)
  })

  it('窗口/标签页：明确说出学生选的是什么，并要求重选「整个屏幕」', () => {
    const windowMessage = describeScreenGateFailure({
      kind: 'capture',
      code: 'SCREEN_NOT_MONITOR',
      surface: 'window',
    })
    expect(windowMessage.title).toContain('应用窗口')
    expect(windowMessage.description).toContain('请重新点击下面的按钮')
    expect(windowMessage.description).toContain('「整个屏幕」')

    const tabMessage = describeScreenGateFailure({
      kind: 'capture',
      code: 'SCREEN_NOT_MONITOR',
      surface: 'browser',
    })
    expect(tabMessage.title).toContain('浏览器标签页')
    expect(tabMessage.description).toContain('不要选择窗口或浏览器标签页')
  })

  it('用户取消：文案说明"必须共享整个屏幕"并给出重试动作', () => {
    const message = describeScreenGateFailure({
      kind: 'capture',
      code: 'SCREEN_PERMISSION_DENIED',
      causeName: 'NotAllowedError',
    })

    expect(message.description).toContain('必须共享整个屏幕')
    expect(message.description).toContain('请重新点击下面的按钮')
  })

  it('共享被结束：给出 §22 的那句"当前课堂要求持续共享整个屏幕"', () => {
    const message = describeScreenGateFailure({ kind: 'capture', code: 'SCREEN_TRACK_ENDED' })

    expect(message.title).toBe('屏幕共享已停止')
    expect(message.description).toContain('当前课堂要求持续共享整个屏幕')
  })

  it('surfaceLabel 把枚举翻译成学生看得懂的中文', () => {
    expect(surfaceLabel('monitor')).toBe('整个屏幕')
    expect(surfaceLabel('window')).toBe('应用窗口')
    expect(surfaceLabel('browser')).toBe('浏览器标签页')
    // 'other'（有值但不认识）与 'unknown'（什么都没说）必须说成两件事。
    expect(surfaceLabel('other')).toBe('其它内容')
    expect(surfaceLabel('unknown')).toBe('无法确认的共享范围')
    expect(surfaceLabel(null)).toBe('无法确认的共享范围')
  })
})
