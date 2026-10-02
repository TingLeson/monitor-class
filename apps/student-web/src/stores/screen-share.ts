import { defineStore } from 'pinia'
import { computed, markRaw, ref, shallowRef } from 'vue'
import {
  checkScreenCaptureSupport,
  describeScreenCapture,
  releaseScreenCapture,
  requestEntireScreen,
  ScreenGateError,
  type ScreenCapture,
  type ScreenCaptureDiagnostics,
  type ScreenCaptureErrorCode,
  type ScreenCaptureUnsupportedReason,
  type ScreenSurface,
} from '../lib/screen-capture'

/**
 * 屏幕共享状态机（§16 / §18 / §21 / §22；docs/frontend/student.md）。
 *
 * ```
 *      start()                 Gate 通过
 * idle ─────────→ requesting ─────────────→ sharing
 *  ↑                  │                        │
 *  │             Gate 拒绝 /               track ended (§22)
 *  │             用户取消                       │
 *  │                  ↓                        ↓
 *  └── reset() ──── error ←──── start() ──── lost
 * ```
 *
 * 三条必须守住的规则：
 *
 * 1. **状态只能由真实事件驱动**。`sharing` 只在拿到 `displaySurface === 'monitor'`
 *    的轨道之后才出现（§18：先 Gate 再进入课堂），绝不在点击的瞬间先把界面切成
 *    "正在共享"——那等于用一个乐观更新假装 Gate 已经通过。
 * 2. **`start()` 不可重入**。进行中的请求再点多少次都只有一次 `getDisplayMedia`
 *    （§57：不要偷偷请求，也不要在学生以为"没反应"时弹出第二个授权窗口）。
 * 3. **轨道必须被显式释放**。丢失、停止、重置、组件卸载都要走 `releaseScreenCapture`，
 *    否则浏览器会一直显示"正在共享"，学生在一节已经退出的课堂里被持续看着。
 */

export type ScreenShareStatus = 'idle' | 'requesting' | 'sharing' | 'lost' | 'error'

/** 失败信息：只保留可序列化的字段（错误对象本身不进状态，见下方 markRaw 说明）。 */
export interface ScreenShareFailure {
  code: ScreenCaptureErrorCode
  surface: ScreenSurface | null
  /** 浏览器原始 error.name（NotAllowedError / NotReadableError / ...），文案层要用。 */
  causeName: string | null
}

export const useScreenShareStore = defineStore('student-screen-share', () => {
  const status = ref<ScreenShareStatus>('idle')

  /**
   * 当前捕获。
   *
   * WHY `shallowRef` + `markRaw`（两个都要，缺一不可）：
   *
   * MediaStream / MediaStreamTrack 是浏览器里的**宿主对象**。Vue 的响应式系统会
   * 深度遍历普通对象并建立 Proxy；一条屏幕轨道背后挂着采集器、编码器、GPU 纹理，
   * 把它变成响应式代理会造成两类真实故障：
   *
   * 1. **功能异常**：`track.onended = fn`、`addEventListener`、`getSettings` 这些
   *    调用经过 Proxy 之后，接收者（receiver）不再是原生对象本身，部分浏览器会
   *    直接抛 `TypeError: Illegal invocation`——表现是"共享能开始，但停止共享永远
   *    检测不到"（§22 的核心功能）。而且 `stream.getTracks()[0] !== track` 这类
   *    身份比较也会因为代理层而失真。
   * 2. **性能**：万级属性的宿主对象被递归代理，每次访问都过一遍依赖收集。
   *
   * `shallowRef` 让 Vue 不去深度转换它的值，`markRaw` 再给对象本身打上"永不代理"
   * 的标记（即使它被塞进别的响应式结构里也安全）。这里用 `markRaw` 而不是只靠
   * `shallowRef`：`markRaw` 是**对象级**保证，不依赖"每个读到它的人都记得用
   * shallowRef"这种约定。
   */
  const capture = shallowRef<ScreenCapture | null>(null)

  /** Gate 通过时的诊断快照（§43 的 join 会带上它；纯枚举值，可序列化）。 */
  const diagnostics = ref<ScreenCaptureDiagnostics | null>(null)
  /** 因能力不足被拒绝的原因（§17）；它发生在请求之前，因此与 failure 分开。 */
  const unsupportedReason = ref<ScreenCaptureUnsupportedReason | null>(null)
  const failure = ref<ScreenShareFailure | null>(null)

  const isRequesting = computed(() => status.value === 'requesting')
  const isSharing = computed(() => status.value === 'sharing')
  /** 曾经共享过、现在断了（§22 → 需要"重新共享整个屏幕"）。 */
  const isLost = computed(() => status.value === 'lost')
  const hasFailed = computed(() => status.value === 'error')

  /** 结束订阅的取消函数。放在闭包里而不是状态里：它不该被视图读，也不该被渲染。 */
  let unsubscribeEnded: (() => void) | null = null

  function detachEndedListener(): void {
    unsubscribeEnded?.()
    unsubscribeEnded = null
  }

  /** 释放当前捕获（幂等）。所有退出路径的唯一出口。 */
  function releaseCurrent(): void {
    detachEndedListener()
    releaseScreenCapture(capture.value)
    capture.value = null
  }

  /**
   * 请求整屏并过 Gate（§18）。
   *
   * 返回 Promise 仅仅是为了让调用方可以 await（视图不需要读返回值，状态就是结果）。
   * **不抛异常**：失败被记录在 `failure` / `unsupportedReason` 里——视图的职责是渲染
   * 状态，而不是在 catch 里再拼一次错误处理逻辑（那样每个调用点都会漏掉一两个字段）。
   */
  async function start(): Promise<void> {
    /**
     * 重入保护必须放在**第一行**（而不是 await 之后）：`status` 是同步赋值的，
     * 因此"点两下"里第二下进来时看到的一定是 'requesting'。这个顺序让"重复点击
     * 只触发一次捕获"成为**状态机**的性质，而不是按钮 disabled 这种可以被绕过
     * （回车键、测试直接调 store）的界面约束。
     */
    if (status.value === 'requesting' || status.value === 'sharing') return

    // 从 lost / error 重新开始：先把上一次的残骸清掉（含未释放的订阅与轨道）。
    releaseCurrent()
    failure.value = null
    unsupportedReason.value = null

    const support = checkScreenCaptureSupport()
    if (!support.supported) {
      unsupportedReason.value = support.reason ?? 'no-getDisplayMedia'
      status.value = 'error'
      return
    }

    status.value = 'requesting'
    try {
      const next = await requestEntireScreen()

      /**
       * 请求飞行期间若状态被别的路径改掉（stop()、reset()、组件卸载），
       * 这条轨道就已经"没有主人"了。此时**必须**把它停掉再返回，
       * 否则我们会得到一条谁也拿不到的 live 轨道 = 学生被共享着却无人能停。
       */
      if (status.value !== 'requesting') {
        releaseScreenCapture(next)
        return
      }

      // markRaw 的调用点：所有进入 state 的浏览器对象都在这里被标记（见 capture 的说明）。
      capture.value = markRaw(next)
      diagnostics.value = describeScreenCapture(next)
      status.value = 'sharing'

      /**
       * §22 客户端一半：浏览器"停止共享"/轨道结束。
       *
       * 实现里已经保证"先停轨道、再通知订阅者"，所以这里的顺序问题只剩状态本身：
       * 回调必须能安全地重复到达（`ended` 可能同时从 `onended` 与
       * `addEventListener` 两个形态到达），靠 `capture.value !== next` 这个身份判断兜住。
       *
       * 边界：如果轨道在订阅这一刻就已经是 ended，`onEnded` 会**同步**回调一次，
       * 于是下面的 `unsubscribeEnded = ...` 是给一个已经结束的订阅赋值。这是安全的——
       * 真实实现在那条路径上返回的是空操作函数，且 `capture.value` 已被清空。
       */
      unsubscribeEnded = next.onEnded(() => {
        if (capture.value !== next) return
        releaseCurrent()
        status.value = 'lost'
        failure.value = {
          code: 'SCREEN_TRACK_ENDED',
          surface: next.surface,
          causeName: 'ended',
        }
      })
    } catch (cause) {
      if (status.value !== 'requesting') return
      const error = cause instanceof ScreenGateError ? cause : null
      failure.value = {
        code: error?.code ?? 'SCREEN_API_UNSUPPORTED',
        surface: error?.surface ?? null,
        causeName: error?.causeName ?? null,
      }
      status.value = 'error'
    }
  }

  /**
   * 主动停止共享（"停止共享"按钮、课堂关闭、离开页面）。
   *
   * 回到 `idle` 而不是 `lost`：`lost` 表达的是"意外断了"（§22 要提示学生重新共享），
   * 而主动停止是学生自己的决定，显示一条"已停止屏幕共享"的警告是在制造假故障。
   */
  function stop(): void {
    releaseCurrent()
    failure.value = null
    unsupportedReason.value = null
    status.value = 'idle'
  }

  /** 重置到初始状态（切换课堂、离开页面时用；与 stop() 等价，语义更明确）。 */
  function reset(): void {
    stop()
  }

  return {
    status,
    capture,
    diagnostics,
    unsupportedReason,
    failure,
    isRequesting,
    isSharing,
    isLost,
    hasFailed,
    start,
    stop,
    reset,
  }
})
