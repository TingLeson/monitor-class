import { afterEach, vi } from 'vitest'
import type { ScreenCapture } from '../lib/screen-capture'

/**
 * 屏幕捕获测试替身（§64 Frontend Test："Screen Gate" 是重点之一）。
 *
 * WHY 手写替身而不是引第三方 mock 库：我们要覆盖的是**浏览器的真实行为差异**——
 * 用户选了什么共享面、`getSettings` 返回不返回 `displaySurface`、track 用哪种形态
 * 通知 ended。这些差异必须由测试显式构造出来（§65 Case 6/7/8 的本质就是三种选择），
 * 一个"什么都返回 monitor"的通用 mock 会把三类拒绝路径全部掩盖掉。
 */

/** 一次 `getDisplayMedia` 调用的完整记录：参数与"用户选了什么"。 */
export interface CapturedCall {
  constraints: unknown
  surface: ScreenSurfaceUnderTest
}

/** 替身能模拟的三种浏览器选择 + 两种信息缺失。 */
export type ScreenSurfaceUnderTest =
  'monitor' | 'window' | 'browser' | 'missing' | 'empty' | 'other'

/** 与真实 MediaStreamTrack 中我们用到的那部分一致的替身。 */
export interface FakeTrack {
  kind: string
  readyState: string
  onended: ((event: Event) => void) | null
  getSettings(): MediaTrackSettings
  stop(): void
  addEventListener(type: string, listener: () => void): void
  removeEventListener(type: string, listener: () => void): void
  /** 测试专用：记录停了几次，用来断言"所有 track 都被 stop"。 */
  readonly stopCalls: number
  /** 测试专用：按真实浏览器的方式派发 ended（走 addEventListener 注册的监听）。 */
  emitEnded(): void
  /** 测试专用：只走 `onended` 属性这一条路径（模拟只支持它的实现）。 */
  emitEndedViaProperty(): void
  /** 测试专用：`getSettings` 抛出（某些实现对非捕获轨道会这么做）。 */
  breakGetSettings(): void
}

function settingsFor(surface: ScreenSurfaceUnderTest): MediaTrackSettings {
  switch (surface) {
    case 'monitor':
      return { displaySurface: 'monitor', width: 2560, height: 1440 }
    case 'window':
      return { displaySurface: 'window' }
    case 'browser':
      return { displaySurface: 'browser' }
    case 'missing':
      // 完全没有 displaySurface 字段：旧实现 / 非屏幕轨道就是这样。
      return { width: 1280, height: 720 }
    case 'empty':
      // 空字符串与缺失是同一件事：浏览器没有告诉我们任何信息。
      return { displaySurface: '' }
    case 'other':
      // 规范把 displaySurface 定成 DOMString：将来完全可能出现第四个值。
      return { displaySurface: 'monitor-ish' }
  }
}

export function makeFakeTrack(surface: ScreenSurfaceUnderTest = 'monitor'): FakeTrack {
  const listeners = new Set<() => void>()
  let stopCalls = 0
  let settings: MediaTrackSettings | null = settingsFor(surface)

  return {
    kind: 'video',
    readyState: 'live',
    onended: null,
    get stopCalls() {
      return stopCalls
    },
    getSettings(): MediaTrackSettings {
      if (settings === null) throw new TypeError('getSettings is not supported')
      return settings
    },
    stop(): void {
      stopCalls += 1
      this.readyState = 'ended'
    },
    addEventListener(type: string, listener: () => void): void {
      if (type === 'ended') listeners.add(listener)
    },
    removeEventListener(type: string, listener: () => void): void {
      if (type === 'ended') listeners.delete(listener)
    },
    emitEnded(): void {
      this.readyState = 'ended'
      for (const listener of [...listeners]) listener()
    },
    emitEndedViaProperty(): void {
      this.readyState = 'ended'
      this.onended?.(new Event('ended'))
    },
    breakGetSettings(): void {
      settings = null
    },
  }
}

export interface FakeStream {
  active: boolean
  getTracks(): FakeTrack[]
  getVideoTracks(): FakeTrack[]
}

export function makeFakeStream(tracks: FakeTrack[], withVideo = true): FakeStream {
  return {
    active: true,
    getTracks: () => [...tracks],
    getVideoTracks: () => (withVideo ? tracks.filter((track) => track.kind === 'video') : []),
  }
}

/**
 * 摄像头轨道替身（§24 / §75）。
 *
 * 刻意复用 `makeFakeTrack('missing')`：它的 `getSettings()` **没有** `displaySurface`
 * ——摄像头代码本来就不该读那个字段（§16 的 Gate 只针对屏幕）。用一个"没有共享面"
 * 的轨道当摄像头替身，等于在测试层面盯住"摄像头这条路径上不存在任何 displaySurface
 * 判断"；哪天有人把两条路径合到一起，这些用例会立刻红。
 */
export function makeFakeCameraTrack(): FakeTrack {
  return makeFakeTrack('missing')
}

/**
 * 摄像头流替身：**必须是真实的 `MediaStream` 实例**。
 *
 * WHY 与 `makeFakeStream`（屏幕用）不同：摄像头开启后页面会把它挂到
 * `<video>.srcObject`，而 happy-dom 对 srcObject 做类型检查（不是 MediaStream
 * 就抛 TypeError）。屏幕那条流刻意不满足这个条件——§56 禁止屏幕预览，它连一个
 * `<video>` 都不该碰到；这条差异本身就是"两种画面的处理规则不同"的证明。
 *
 * happy-dom 漏实现了 `getTracks()`（真实浏览器有），这里按规范补上：少了它，
 * `releaseCamera` 的 stopAllTracks 会在测试里抛错，而生产路径不可能失败——
 * 那会把"关掉摄像头必须停轨道"变成一次测试环境的假故障。
 */
export function makeFakeCameraStream(tracks: FakeTrack[] = [makeFakeCameraTrack()]): FakeStream {
  const stream = new MediaStream(tracks as unknown as MediaStreamTrack[])
  Object.defineProperty(stream, 'getTracks', {
    value: () => [...tracks],
    configurable: true,
  })
  return stream as unknown as FakeStream
}

export interface MediaDevicesStub {
  calls: CapturedCall[]
  /**
   * 每次调用返回的流，按顺序排列。
   *
   * WHY 要留下来：Phase 6 最关键的断言是"publish 的是**同一条**轨道"。
   * 只有能拿到替身造出来的那条 track，才能把它和 publish 的入参做身份比较
   * （而不是比较一个宽泛的"被调用过"）。
   */
  streams: FakeStream[]
  /** 下一次（以及之后）的选择；要在中途换结果就直接改这个字段。 */
  surface: ScreenSurfaceUnderTest
  /** 让 getDisplayMedia 抛出这个错误（模拟用户取消 / 系统拒绝 / 其它失败）。 */
  failWith: Error | null
  /** 每次调用前执行，用来构造"第 N 次不同"的场景。 */
  beforeCall: (() => void) | null
  getDisplayMedia(constraints?: unknown): Promise<unknown>

  /* ------------------------------------------------------------------ */
  /* 摄像头（§24）                                                        */
  /* ------------------------------------------------------------------ */

  /**
   * 每一次 `getUserMedia` 的入参。
   *
   * 数组长度就是本 Phase 最核心的一条断言的来源：**进入课堂前与挂载时必须是 0**，
   * 只有学生点了按钮才允许变成 1。用长度而不是一个布尔，才能同时断言"开/关/再开"
   * 是三次独立的请求。
   */
  cameraCalls: { constraints: unknown }[]
  /** 每次返回的摄像头流（与 cameraCalls 一一对应）。 */
  cameraStreams: FakeStream[]
  /** 让 getUserMedia 抛出这个错误（权限被拒 / 设备被占用）。 */
  cameraFailWith: Error | null
  getUserMedia(constraints?: unknown): Promise<unknown>
}

export function makeMediaDevicesStub(
  surface: ScreenSurfaceUnderTest = 'monitor',
): MediaDevicesStub {
  const stub: MediaDevicesStub = {
    calls: [],
    streams: [],
    surface,
    failWith: null,
    beforeCall: null,
    getDisplayMedia(constraints?: unknown): Promise<unknown> {
      stub.beforeCall?.()
      stub.calls.push({ constraints, surface: stub.surface })
      if (stub.failWith) return Promise.reject(stub.failWith)
      const stream = makeFakeStream([makeFakeTrack(stub.surface)])
      stub.streams.push(stream)
      return Promise.resolve(stream)
    },
    cameraCalls: [],
    cameraStreams: [],
    cameraFailWith: null,
    getUserMedia(constraints?: unknown): Promise<unknown> {
      stub.cameraCalls.push({ constraints })
      if (stub.cameraFailWith) return Promise.reject(stub.cameraFailWith)
      const stream = makeFakeCameraStream()
      stub.cameraStreams.push(stream)
      return Promise.resolve(stream)
    },
  }
  return stub
}

/**
 * 把替身装到 `navigator.mediaDevices` 上。
 *
 * happy-dom 默认**没有** `navigator.mediaDevices`（这正是 §17 能力自检要检测的
 * 第一种情况），所以这里用 defineProperty 直接定义；每个测试结束后由
 * `cleanupMediaDevices()` 还原，避免"某个测试留下的 mediaDevices 让另一个测试
 * 的能力自检悄悄通过"——那种污染会让一组测试整体失去意义。
 */
export function installMediaDevices(devices: unknown): void {
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    writable: true,
    value: devices,
  })
}

export function cleanupMediaDevices(): void {
  Reflect.deleteProperty(navigator, 'mediaDevices')
}

/** 自动清理：任何安装过 mediaDevices 的测试都不会污染下一个。 */
afterEach(() => {
  cleanupMediaDevices()
  vi.restoreAllMocks()
})

/**
 * 组件测试用的假捕获对象（`requestEntireScreen` 的返回值形状）。
 *
 * WHY 组件测试要 mock 掉 `screen-capture.ts` 而不是继续用 mediaDevices 替身：
 * 组件关心的是"Gate 结果如何映射到界面"，而 Gate 判定逻辑已经在
 * `screen-capture.spec.ts` 里逐条覆盖过。两层各测各的，失败时能立刻定位在
 * "Gate 判错了"还是"界面渲染错了"。
 */
export function makeFakeCapture(
  options: {
    surface?: 'monitor'
    onStop?: () => void
  } = {},
): ScreenCapture & { emitEnded: () => void } {
  const track = makeFakeTrack('monitor')
  const listeners: (() => void)[] = []
  let stopped = false

  const capture: ScreenCapture & { emitEnded: () => void } = {
    stream: makeFakeStream([track]) as unknown as MediaStream,
    track: track as unknown as MediaStreamTrack,
    surface: 'monitor',
    settings: { displaySurface: 'monitor', rawDisplaySurface: 'monitor' },
    stop(): void {
      if (stopped) return
      stopped = true
      track.stop()
      options.onStop?.()
    },
    onEnded(listener: () => void): () => void {
      /**
       * 已经结束的轨道不会再触发 `ended`：直接回调一次。
       *
       * 这与真实实现（`screen-capture.ts`）的契约一致，也是会话页必须处理的一个
       * 真实窗口——轨道可能在"订阅监听"之前就被浏览器结束了。替身少了这一条，
       * 那个窗口就永远不会被测到。
       */
      if (stopped || track.readyState === 'ended') {
        listener()
        return () => undefined
      }
      listeners.push(listener)
      return () => {
        const index = listeners.indexOf(listener)
        if (index >= 0) listeners.splice(index, 1)
      }
    },
    emitEnded(): void {
      // 先停轨道再通知，与真实实现（screen-capture.ts）的顺序一致。
      capture.stop()
      for (const listener of [...listeners]) listener()
    },
  }
  return capture
}
