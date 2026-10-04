/**
 * 整屏共享 Gate（§16 / §17 / §18 / §22 Step 3 / §71）。
 *
 * 本文件是整个项目**唯一**允许调用 `getDisplayMedia` 的地方。这样做的原因不是
 * 为了"整洁"，而是因为 §71 把"只能共享整块显示器"定为业务不变量：一旦有第二个
 * 调用点，就等于有了第二条可以绕过 Gate 的路径，而绕过它的学生看起来和正常学生
 * 完全一样（老师那端只会看到"画面是某个窗口"）。
 *
 * 本 Phase 的边界（§71）：只做
 * `getDisplayMedia → MediaStream → MediaStreamTrack → getSettings → displaySurface → onended`。
 * 不接 LiveKit、不调任何后端接口；把轨道 publish 到课堂是 Phase 6（§20）。
 *
 * 三层防御的先后顺序（顺序本身就是设计，不能重排）：
 *
 * 1. **能力自检**（`checkScreenCaptureSupport`，§17）——在请求之前。浏览器根本
 *    没有 Screen Capture API 时，绝不能"试一下再说"：那会弹出一个浏览器自己的
 *    报错，学生看到的是英文技术信息，而我们要给的是"请换 Chrome / Edge"。
 * 2. **约束偏好**（`SCREEN_CAPTURE_CONSTRAINTS`，§16）——请求时表达"我要整块屏幕"。
 *    必须清楚：约束**只是偏好**。Chrome 的 UI 永远让学生自己选 monitor / window /
 *    browser，并且 `displaySurface` 不是强制约束（没有浏览器会因为你不满足它而拒绝
 *    授权），所以约束能提高"默认选中整个屏幕"的概率，但**不能**作为判据。
 * 3. **`displaySurface` 硬 Gate**（`requestEntireScreen`，§16）——真正的业务闸门。
 *    只有它看到 `'monitor'` 才放行；其余一律 `track.stop()` + 拒绝。
 */

import type { CaptureDiagnostics } from '@classwatch/shared-types'

/** 屏幕捕获相关的本地错误码（§58 下半部分，已在 shared-types 登记）。 */
export type ScreenCaptureErrorCode =
  | 'SCREEN_PERMISSION_DENIED'
  | 'SCREEN_NOT_MONITOR'
  | 'SCREEN_API_UNSUPPORTED'
  | 'SCREEN_TRACK_ENDED'

/**
 * 枚举值的唯一来源：既是类型，也是运行期白名单。
 * `messages` 里用 `Record<ScreenCaptureErrorCode, ...>` 承接，新增码而忘记写文案
 * 会直接编译失败，而不是在课堂上显示一句空白。
 */
export const SCREEN_CAPTURE_ERROR_CODES = [
  'SCREEN_PERMISSION_DENIED',
  'SCREEN_NOT_MONITOR',
  'SCREEN_API_UNSUPPORTED',
  'SCREEN_TRACK_ENDED',
] as const

/** 能力自检失败的三种具体原因（§17 逐项检查）。 */
export type ScreenCaptureUnsupportedReason =
  /**
   * 页面不在**安全上下文**里（http:// 的局域网 IP 或域名）。
   *
   * WHY 必须与 `no-mediaDevices` 分开：在 http:// 下 `navigator.mediaDevices`
   * **就是** undefined，两者在代码层长得一模一样，但学生的动作完全相反——
   * 换浏览器没有用，必须换成 https:// 或 localhost。真实课堂测试里就有人
   * 被"请使用最新版 Chrome 或 Edge"骗去升级浏览器，而问题在地址栏。
   */
  'insecureContext' | 'no-mediaDevices' | 'no-getDisplayMedia' | 'no-getSettings'

/**
 * `getSettings().displaySurface` 的取值。
 *
 * 写成字符串联合而不是 `'monitor' | 'window' | 'browser'`：`displaySurface` 在
 * 规范里的类型是 `DOMString`，浏览器未来完全可能返回第四个值。把未知值也纳入
 * 类型，才能让"未知值一律当非 monitor"这条规则在类型层面就成立。
 *
 * `'other'`（有值但不认识）与 `'unknown'`（没有值）必须分开：
 * - `'other'` = 浏览器明确告诉我们"这不是整块屏幕的标准取值" → `SCREEN_NOT_MONITOR`；
 * - `'unknown'` = 浏览器什么都没说（缺失 / 空串） → `SCREEN_API_UNSUPPORTED`。
 * 两者的学生动作完全不同（重选 vs 换浏览器），混成一个值就再也说不清该做什么。
 */
export type ScreenSurface = 'monitor' | 'window' | 'browser' | 'other' | 'unknown'

export interface ScreenCaptureSupport {
  supported: boolean
  /** 仅在 supported === false 时有意义（§17 要能说清是缺了哪一项）。 */
  reason?: ScreenCaptureUnsupportedReason
}

export interface ScreenCaptureSettings {
  displaySurface: ScreenSurface
  /** 浏览器原始返回值；自检/诊断用，允许 undefined。 */
  rawDisplaySurface?: string
  /** 声明这块画面是否还能继续产生新帧（`getSettings()` 里少见的**强制**约束）。 */
  surfaceActive?: boolean
  /**
   * 画面尺寸（像素）。
   *
   * WHY 放在 Gate 的产物里而不是 publish 时现读：§43 的 join 请求体要求提交
   * `{displaySurface, width, height}` 作为诊断。`getSettings()` 在轨道已经 ended
   * 之后会抛 `InvalidStateError`，而 join 恰好可能发生在"Gate 通过、还没来得及
   * 提交"的窗口里；在 Gate 那一刻把值快照下来，是唯一不会在半路失败的时机。
   */
  width?: number
  height?: number
}

export interface ScreenCapture {
  stream: MediaStream
  track: MediaStreamTrack
  /** Gate 的结论。任何时候都只可能是 'monitor'——否则根本不会构造出这个对象。 */
  surface: ScreenSurface
  settings: ScreenCaptureSettings
  /** 幂等：重复调用不会报错，也不会重复触发 ended。 */
  stop(): void
  /**
   * 订阅"共享被结束"（§22）。返回**取消订阅**函数。
   *
   * WHY 由 lib 自己管两种事件形态：`track.onended = fn` 与
   * `track.addEventListener('ended', fn)` 都能收到浏览器"停止共享"，但前者会被
   * 后来的赋值覆盖、后者需要显式 remove。把这层差异关在本文件里，store 与视图
   * 就只需要处理"结束了"这一个事实。
   *
   * 同一时刻只应有一个订阅者：内部实现共用一份 `onended` 属性（见下），
   * 交接所有权时要先取消订阅再重新订阅（Phase 6 的 PreJoin → Session 交接即如此）。
   */
  onEnded(listener: () => void): () => void
}

/**
 * 诊断信息（§43 的 join 请求会带上它）。
 *
 * 刻意**不含** track / stream / deviceId：只有能被 JSON 序列化的枚举快照。
 * 浏览器对象一旦被写进会被序列化或被持久化的地方，就是一个泄漏面。
 */
export interface ScreenCaptureDiagnostics {
  displaySurface: ScreenSurface
  rawDisplaySurface: string | null
  surfaceActive: boolean | null
  width: number | null
  height: number | null
}

/* -------------------------------------------------------------------------- */
/* 约束常量（§16 prefer monitor / include monitor / avoid surface switching）  */
/* -------------------------------------------------------------------------- */

/**
 * 任务书 §16 未展开、但 Chrome 实际支持的扩展约束字段。
 *
 * WHY 手写类型扩展而不是 `as any`：TS 的 `lib.dom.d.ts` 只声明了
 * `DisplayMediaStreamOptions { audio, video }`，而这里要用到的 `selfBrowserSurface`、
 * `surfaceSwitching`、`systemAudio`、`monitorTypeSurfaces`、`preferCurrentTab` 以及
 * `video.displaySurface` 都属于屏幕捕获规范的后续扩展。写一个显式接口，好处是
 * 拼错字段名（`surfaceSwiching`）会编译失败，而 `as any` 会让它静默失效——
 * 静默失效在这里的后果是"约束没生效"，也就是学生更容易选错共享面。
 */
interface ExtendedDisplayMediaStreamOptions extends DisplayMediaStreamOptions {
  video?: MediaTrackConstraints & { displaySurface?: ConstrainDOMString }
  selfBrowserSurface?: 'include' | 'exclude'
  surfaceSwitching?: 'include' | 'exclude'
  systemAudio?: 'include' | 'exclude'
  monitorTypeSurfaces?: 'include' | 'exclude'
  preferCurrentTab?: boolean
}

/**
 * 请求整屏时的约束（§16：prefer monitor / include monitor / avoid surface switching）。
 *
 * 每一项的 WHY：
 * - `video.displaySurface: 'monitor'`：表达偏好，让 Chrome 的选择器默认落在
 *   "整个屏幕"上。它**不是** Gate——不满足它浏览器照样授权。
 * - `selfBrowserSurface: 'exclude'`：不把"本标签页"列为选项。否则学生可以共享
 *   我们自己的 PreJoin 页面，屏幕共享就变成了"共享一个静态说明页"，完全没有监督意义。
 * - `surfaceSwitching: 'exclude'`：共享过程中不允许换共享面。允许切换就等于允许
 *   "先用整个屏幕过 Gate，再切成一个窗口"，那 Gate 只是一次性的表演。
 * - `systemAudio: 'exclude'`：本 Phase 只做屏幕画面（音频属 §25，且学生麦克风另走
 *   一条链路）。要求学生额外授权系统音频会把一次授权变成两次。
 * - `monitorTypeSurfaces: 'include'`：**保留**"整个屏幕"选项。默认值虽然就是 include，
 *   但显式写出来是为了让"整屏必须可选"这件事在读代码时是可见的——一旦有人把它改成
 *   exclude，本 Gate 就永远不可能通过。
 * - `preferCurrentTab: false`：与 `selfBrowserSurface: exclude` 同向，防止浏览器
 *   把本标签页作为首选。
 */
export const SCREEN_CAPTURE_CONSTRAINTS: ExtendedDisplayMediaStreamOptions = {
  video: { displaySurface: 'monitor' },
  audio: false,
  selfBrowserSurface: 'exclude',
  surfaceSwitching: 'exclude',
  systemAudio: 'exclude',
  monitorTypeSurfaces: 'include',
  preferCurrentTab: false,
}

/* -------------------------------------------------------------------------- */
/* 错误                                                                        */
/* -------------------------------------------------------------------------- */

export class ScreenGateError extends Error {
  readonly code: ScreenCaptureErrorCode
  /** Gate 实际读到的共享面；能力/权限失败时为 null（根本没拿到轨道）。 */
  readonly surface: ScreenSurface | null
  /**
   * 浏览器抛出的原始 `error.name`（NotAllowedError / NotReadableError / ...）。
   *
   * WHY 不像 §58 那样只留错误码：`SCREEN_PERMISSION_DENIED` 同时覆盖"学生点了取消"
   * 与"macOS 没有授予屏幕录制权限"两种情况，而这两句话要给学生完全不同的下一步
   * （一个是"重新点一次并选择整个屏幕"，一个是"去系统设置里授权"）。
   * 码必须保持 §58 的契约不变，因此把浏览器的原因**附加**在这里，由文案层决定怎么说。
   */
  readonly causeName: string | null

  constructor(
    code: ScreenCaptureErrorCode,
    options: {
      surface?: ScreenSurface | null
      causeName?: string | null
      message?: string
      cause?: unknown
    } = {},
  ) {
    super(
      options.message ?? code,
      options.cause === undefined ? undefined : { cause: options.cause },
    )
    this.name = 'ScreenGateError'
    this.code = code
    this.surface = options.surface ?? null
    this.causeName = options.causeName ?? null
  }
}

/* -------------------------------------------------------------------------- */
/* 能力自检（§17，发生在任何捕获请求之前）                                      */
/* -------------------------------------------------------------------------- */

/**
 * §17 的 Capability Check。
 *
 * 检查三项：`navigator.mediaDevices`、`getDisplayMedia`、`MediaStreamTrack.getSettings`。
 *
 * WHY 连 `getSettings` 都要检查，而不是"拿到流再说"：没有 `getSettings` 就**不可能**
 * 确认共享面，而 §16 明确要求"无法确认时同样拒绝"。提前拒绝的价值在于学生不会先
 * 经历一次真实的授权弹窗（有的系统上弹窗带着"正在共享你的屏幕"的系统提示），
 * 再被告知"这个浏览器不行"——那时共享其实已经发生过了。
 *
 * 这是纯读取检查，**不产生任何副作用**：不会请求权限、不会弹窗。
 */
export function checkScreenCaptureSupport(): ScreenCaptureSupport {
  // 顺序很关键：安全上下文必须**先**判。不安全时 mediaDevices 本来就是 undefined，
  // 放到后面判会永远命中 no-mediaDevices，把"地址不对"误报成"浏览器太旧"。
  if (typeof window !== 'undefined' && window.isSecureContext === false) {
    return { supported: false, reason: 'insecureContext' }
  }
  const devices: MediaDevices | undefined = navigator.mediaDevices
  if (!devices) return { supported: false, reason: 'no-mediaDevices' }
  if (typeof devices.getDisplayMedia !== 'function') {
    return { supported: false, reason: 'no-getDisplayMedia' }
  }
  /**
   * 检查原型上的 `getSettings`，而不是拿一个真实 track 去试。
   *
   * WHY 走 `globalThis` + 原型：某些实现把 `getSettings` 放在
   * `MediaStreamTrack.prototype` 上（而不是每个实例上），只看实例属性会误判；
   * 而直接引用全局 `MediaStreamTrack` 在极老的浏览器上会让整页抛 ReferenceError——
   * 自检函数本身把页面搞崩，比"检测不到"糟糕得多。
   */
  const trackConstructor: unknown = Reflect.get(globalThis, 'MediaStreamTrack')
  if (typeof trackConstructor !== 'function') return { supported: false, reason: 'no-getSettings' }
  const prototype: unknown = Reflect.get(trackConstructor, 'prototype')
  const getSettings: unknown =
    typeof prototype === 'object' && prototype !== null
      ? Reflect.get(prototype, 'getSettings')
      : undefined
  if (typeof getSettings !== 'function') return { supported: false, reason: 'no-getSettings' }
  return { supported: true }
}

/* -------------------------------------------------------------------------- */
/* 请求 + 硬 Gate（§16 / §18）                                                 */
/* -------------------------------------------------------------------------- */

/**
 * 把浏览器的原始 `displaySurface` 归一化。
 *
 * WHY `''` 与 `undefined` 都当 `'unknown'`：它们是同一件事——浏览器没有告诉我们
 * 任何信息。任何"看起来像有值"的处理都会让 Gate 在信息缺失时放行。
 */
function normalizeSurface(raw: unknown): ScreenSurface {
  if (raw === 'monitor' || raw === 'window' || raw === 'browser') return raw
  if (typeof raw === 'string' && raw !== '') return 'other'
  return 'unknown'
}

/**
 * `getSettings()` 的返回值扩展。
 *
 * `lib.dom.d.ts` 的 `MediaTrackSettings` 里同样没有 `surfaceActive`——它是屏幕捕获
 * 规范后续加入的字段（Chrome 已实现）。这里用交叉类型显式声明，理由与约束那份
 * 类型扩展完全相同：**不用 `as any`**，让字段名拼错在编译期暴露。
 */
interface ExtendedMediaTrackSettings extends MediaTrackSettings {
  /** 这块共享面是否还能产生新帧（`getSettings()` 里少见的**强制**约束）。 */
  surfaceActive?: boolean
}

/** 只挑出我们需要的那几个字段，浏览器对象不进入任何状态/存储（§43）。 */
function readSettings(settings: ExtendedMediaTrackSettings): ScreenCaptureSettings {
  const raw = settings.displaySurface
  return {
    displaySurface: normalizeSurface(raw),
    rawDisplaySurface: typeof raw === 'string' ? raw : undefined,
    surfaceActive: settings.surfaceActive,
    width: normalizeDimension(settings.width),
    height: normalizeDimension(settings.height),
  }
}

/**
 * 尺寸归一化：只有"有限的正数"才算拿到了尺寸。
 *
 * WHY 不直接用 `settings.width`：浏览器可能给 `undefined`，也可能给 0（某些实现
 * 在捕获刚开始、还没有第一帧时就是这样）。把 0 当尺寸提交给后端，会让诊断数据
 * 里出现一堆"学生共享了 0×0 屏幕"的假记录，比缺字段更难排查。
 */
function normalizeDimension(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? Math.round(value)
    : undefined
}

/** 幂等地停掉一批轨道：`stop()` 本身可重复调用，这里只是省去调用方的判空。 */
function stopAllTracks(tracks: readonly MediaStreamTrack[]): void {
  for (const track of tracks) {
    // 逐个 try：某个 track.stop() 抛错不能导致后面的 track 留在 live 状态——
    // 只要有一条轨道没被停掉，浏览器就仍然显示"正在共享"，学生也仍然被看见。
    try {
      track.stop()
    } catch {
      // 停不掉也没有别的补救手段；绝不能因为这里抛错而让上层跳过其余轨道。
    }
  }
}

/**
 * 尽力停掉"半成品"流。
 *
 * WHY 需要这个防御：`getDisplayMedia` 失败时理论上不会返回流，但浏览器实现里
 * "已创建 MediaStream 再报错"并非不可能。此时唯一能保证"没有残留共享"的办法
 * 就是检查参数本身。
 */
function stopEverything(candidate: unknown): void {
  if (
    typeof candidate === 'object' &&
    candidate !== null &&
    'getTracks' in candidate &&
    typeof candidate.getTracks === 'function'
  ) {
    const tracks = (candidate as MediaStream).getTracks()
    if (Array.isArray(tracks)) stopAllTracks(tracks)
  }
}

/**
 * 浏览器抛错 → §58 本地错误码。
 *
 * 映射规则（顺序有意义，`NotAllowedError` 必须先判）：
 * - `NotAllowedError` / `SecurityError`：学生点了"取消"，或系统/策略拒绝授权 →
 *   `SCREEN_PERMISSION_DENIED`。
 * - `NotReadableError`：授权**通过了**，但系统层面无法开始捕获。macOS 上最常见
 *   的成因正是"系统设置 → 隐私与安全性 → 屏幕录制"里没有勾选浏览器（Chrome 154
 *   实测就是这个结果）。它**不是**选错共享面，绝不能报成 SCREEN_NOT_MONITOR——
 *   那会让学生反复重选"整个屏幕"却永远失败。
 * - `AbortError`：Chrome 在"用户没有任何选择就关掉了选择器"时抛它，语义上等同取消。
 * - `InvalidStateError`：常见于不在用户手势里调用。归到 API 层失败，文案会告诉
 *   学生重新点一次按钮（我们的调用始终发生在 click 处理函数内，见视图）。
 * - 其余（含约束字典被拒时的 `TypeError` / `NotSupportedError`）：一律
 *   `SCREEN_API_UNSUPPORTED`。能力自检通过、调用却失败 = 这个浏览器的屏幕捕获
 *   不可用；宁可让少数异常显示成"换浏览器"，也不要让任何一种失败悄悄通过
 *   Gate——Gate 的默认方向必须是拒绝。
 *
 * WHY 不做"约束被拒就换成空约束重试一次"这种兜底：那等于把"这台浏览器不认
 * prefer monitor / avoid surface switching"变成了沉默的降级，而我们随后就只能
 * 靠 displaySurface 兜底——恰恰是 §16 要求尽力避免的局面。宁可明确地报"换 Chrome
 * 或 Edge"（学生有确定的下一步），也不要让一台不认这些偏好的浏览器装作没事。
 *
 * 刻意**不复用** `ApiError`：§58 把后端业务错误与前端本地错误分成两组，
 * 前端媒体错误没有 requestId、也没有后端 message，混用会让视图分不清
 * "要不要重试 / 要不要重新登录"。
 */
const PERMISSION_DENIED_ERROR_NAMES = new Set([
  'NotAllowedError',
  'SecurityError',
  'NotReadableError',
  'AbortError',
])

export function toScreenGateError(cause: unknown): ScreenGateError {
  const name = cause instanceof Error ? cause.name : null
  if (name !== null && PERMISSION_DENIED_ERROR_NAMES.has(name)) {
    // 码相同、原因不同：文案层按 causeName 区分"你取消了"与"系统没给权限"。
    return new ScreenGateError('SCREEN_PERMISSION_DENIED', { causeName: name, cause })
  }
  return new ScreenGateError('SCREEN_API_UNSUPPORTED', { causeName: name, cause })
}

/** 共享面归一化后的中文名（诊断与拒绝文案共用，例如"整个屏幕"）。 */
export function surfaceLabel(surface: ScreenSurface | null): string {
  switch (surface) {
    case 'monitor':
      return '整个屏幕'
    case 'window':
      return '应用窗口'
    case 'browser':
      return '浏览器标签页'
    /**
     * 'other' 与 'unknown' 分开说：前者是"浏览器给了一个我们不认识的共享范围"
     * （拒绝理由成立，应当重新选择），后者是"浏览器什么都没说"（该换浏览器）。
     * 对 'other' 刻意不显示原始取值——它是浏览器内部字符串，学生看不懂，
     * 而诊断需要的那份原始值在 `capture.settings.rawDisplaySurface` 里。
     */
    case 'other':
      return '其它内容'
    default:
      return '无法确认的共享范围'
  }
}

/**
 * `getDisplayMedia` + `displaySurface` 硬 Gate（§16 / §18）。
 *
 * 成功时返回的 `ScreenCapture.surface` 一定是 `'monitor'`；**所有**失败路径都保证
 * 已经把这批轨道全部 stop 掉。这不是"顺手清理"：浏览器只在最后一条活着的屏幕轨道
 * 被 stop 之后才收起"正在共享"提示条，漏掉一条就等于让学生在没有进入课堂的情况下
 * 继续被录制——Gate 拒绝了学生，却把屏幕交了出去。
 */
export async function requestEntireScreen(): Promise<ScreenCapture> {
  const support = checkScreenCaptureSupport()
  if (!support.supported) {
    // 连 API 都没有：绝不发起请求，也不降级成"窗口共享也能进"。
    throw new ScreenGateError('SCREEN_API_UNSUPPORTED', {
      causeName: support.reason ?? null,
    })
  }

  const devices = navigator.mediaDevices
  let stream: MediaStream
  try {
    stream = await devices.getDisplayMedia(SCREEN_CAPTURE_CONSTRAINTS)
  } catch (cause) {
    stopEverything(cause)
    throw toScreenGateError(cause)
  }

  const tracks = stream.getTracks()
  const track = stream.getVideoTracks()[0]

  /**
   * 没有视频轨道：拿不到任何 displaySurface 可读的东西。
   *
   * WHY 也要 `stop()`：`getTracks()` 可能包含音频等其它轨道（当前 `audio: false`
   * 时不会，但约束是偏好、未来可能变），留着它们同样会维持"正在共享"的浏览器提示。
   */
  if (!track) {
    stopAllTracks(tracks)
    throw new ScreenGateError('SCREEN_TRACK_ENDED', { causeName: 'no-video-track' })
  }

  const settings = readSettings(track.getSettings())

  /**
   * 硬 Gate：只有 `'monitor'` 通过（§16）。
   *
   * WHY `undefined` / 缺失也拒绝，而不是"当作 monitor 放行"：严格监控模式下，
   * "无法确认"与"确认不是整屏"对学生而言是同一件事——老师那端看到的东西完全一样。
   * 默认放行等于把 Gate 的默认方向反过来，那样任何能让 displaySurface 缺失的浏览器
   * 都成了绕过路径（§19 的威胁模型只排除"改造前端的高对抗攻击者"，
   * 不包括"正常用户误选"与"故意偷懒"，而后者正是靠这里挡住的）。
   */
  if (settings.displaySurface !== 'monitor') {
    stopAllTracks(tracks)
    throw new ScreenGateError(
      settings.displaySurface === 'unknown' ? 'SCREEN_API_UNSUPPORTED' : 'SCREEN_NOT_MONITOR',
      { surface: settings.displaySurface, causeName: 'displaySurface' },
    )
  }

  /* ------------------------------------------------------------------ */
  /* Gate 通过                                                            */
  /* ------------------------------------------------------------------ */

  let stopped = false
  let unsubscribeEnded: (() => void) | null = null

  const stop = (): void => {
    // 幂等：先摘监听再停轨道，避免 track.stop() 与 onended 互相触发形成回声。
    unsubscribeEnded?.()
    unsubscribeEnded = null
    if (stopped) return
    stopped = true
    stopAllTracks(tracks)
  }

  const capture: ScreenCapture = {
    stream,
    track,
    surface: settings.displaySurface,
    settings,
    stop,
    onEnded(listener) {
      // 已经结束的轨道不会再触发 ended：直接回调一次，否则视图会永远停在
      // "正在共享"而实际早就断了（后加入的订阅者收不到历史事件）。
      if (stopped || track.readyState === 'ended') {
        listener()
        return () => undefined
      }

      const handleEnded = (): void => {
        // §22 客户端一半：立刻释放。不释放的话"已停止共享"的界面背后仍留着
        // 一个 live track，浏览器提示条不会消失，学生会以为自己还在被看着。
        stop()
        listener()
      }

      track.addEventListener('ended', handleEnded)

      /**
       * 兼容 `track.onended = fn` 形态。
       *
       * WHY 两种都挂：`ended` 事件由浏览器自己决定用哪种派发路径（真实 Chrome 两条
       * 都会走，但早期实现/happy-dom 这类替身只支持其中之一）。两个来源都指向同一个
       * `handleEnded`，而它是幂等的（`stop()` 幂等 + 视图侧只关心"是否已经 lost"），
       * 因此重复触发不会造成第二次状态迁移。
       *
       * 注意：这里会**覆盖**调用方自己设置的 onended。本文件是唯一允许持有屏幕
       * 轨道的地方（见文件头），所以这个覆盖是有意的、也是安全的。
       */
      track.onended = handleEnded

      unsubscribeEnded = () => {
        track.removeEventListener('ended', handleEnded)
        if (track.onended === handleEnded) track.onended = null
      }
      return unsubscribeEnded
    },
  }

  return capture
}

/**
 * 释放捕获（§22 / §65 Case 14）。
 *
 * 幂等，且对 `null` / `undefined` 安全：视图卸载、课堂关闭、Gate 失败重试都会调用它，
 * 调用方不该每次先判空。
 */
export function releaseScreenCapture(capture: ScreenCapture | null | undefined): void {
  if (!capture) return
  capture.stop()
}

/** 把捕获对象压成可序列化的诊断信息（§43 的 join 请求用）。 */
export function describeScreenCapture(capture: ScreenCapture): ScreenCaptureDiagnostics {
  return {
    displaySurface: capture.settings.displaySurface,
    rawDisplaySurface: capture.settings.rawDisplaySurface ?? null,
    surfaceActive: capture.settings.surfaceActive ?? null,
    width: capture.settings.width ?? null,
    height: capture.settings.height ?? null,
  }
}

/**
 * §43 冻结的 join 诊断字段：`{ displaySurface, width, height }`。
 *
 * WHY 要在这里"砍掉"上面那些更丰富的诊断值：请求体是**冻结契约**，后端按严格
 * 模式解析（未知字段 400）。rawDisplaySurface / surfaceActive 对排查很有用，
 * 但它们属于客户端日志与界面，不属于这份契约——多塞一个字段的代价是这位学生
 * 根本进不去课堂。
 *
 * 缺失的尺寸用 0 表示（而不是省略字段）：后端契约里 width/height 是 number。
 * 0 的含义在诊断侧是明确的"浏览器没报告尺寸"，与"共享了一块 0×0 的屏幕"
 * 不可能混淆——Gate 已经保证共享的是整块物理显示器。
 */
export function toJoinCaptureDiagnostics(
  diagnostics: ScreenCaptureDiagnostics,
): CaptureDiagnostics {
  return {
    displaySurface: diagnostics.displaySurface,
    width: diagnostics.width ?? 0,
    height: diagnostics.height ?? 0,
  }
}
