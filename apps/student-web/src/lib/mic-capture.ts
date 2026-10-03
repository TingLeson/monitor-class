/**
 * 麦克风采集（§25 / §58 的前端本地错误码 / §76）。
 *
 * 与 `camera-capture.ts` 是**刻意分开**的两份，而且这个文件里没有、也永远不该有
 * `video` 约束。三条理由都是业务不变量：
 *
 * 1. **摄像头与麦克风是两种可选设备**（§21/§24/§25）。合并成一个
 *    `getUserMedia({video:true, audio:true})` 就等于学生点一次摄像头、浏览器同时
 *    要麦克风权限——那是他没点过的一次授权（§57 的隐私 UX 不允许）。
 * 2. **失败模式不同**。摄像头被占用是最常见的失败（会议软件抢设备），而麦克风
 *    更常见的是"系统给了权限但设备静音/被独占"；把两者的文案表合并，早晚会出现
 *    "摄像头被占用"这句话出现在麦克风失败上。
 * 3. **本地静音只有麦克风有**（§31）。学生要能在私密语音里"静音自己"，
 *    而那是一个**不停止设备**的动作（`track.enabled = false`）——摄像头没有这个概念。
 *
 * 这个文件只负责"拿到一条 audio track / 把浏览器错误翻成可执行的下一步"，
 * 状态机（off / requesting / on / error）与 publish 在 `stores/media-session.ts`：
 * 状态是**会话**的属性（要跨视图存活、要在离开课堂时统一释放），不是采集的属性。
 */

/**
 * 麦克风状态机（§25 / §56）。
 *
 * ```
 *        enableMicrophone()           拿到轨道 + publish 成功
 *  off ───────────────────→ requesting ─────────────────────→ on
 *   ↑                            │                              │
 *   │                      权限被拒 / 设备占用 /            disableMicrophone()
 *   │                      浏览器不支持 / 轨道结束                │
 *   │                            ↓                              │
 *   └──── enableMicrophone() ── error ←─────────────────────────┘
 * ```
 *
 * WHY 四个状态而不是一个布尔：`requesting` 与 `off` 必须分开，否则"点了没反应"
 * 的那几百毫秒里按钮是活的，学生会连点，而每次点击都是一次新的设备请求；
 * `error` 与 `off` 必须分开，因为"没开"和"打不开"要给完全不同的话（后者要给下一步动作）。
 *
 * 这个状态**不属于** `MediaSessionPhase`：麦克风开或关都不会让会话变成
 * ONLINE / SCREEN_LOST（§21：屏幕才是 mandatory；§25 只要求"学生主动开启以后"发布轨道）。
 */
export const MIC_STATES = ['off', 'requesting', 'on', 'error'] as const

export type MicState = (typeof MIC_STATES)[number]

/**
 * 麦克风错误类别。
 *
 * WHY 复用 §58 的 `MIC_PERMISSION_DENIED` 而不是自造一套完整错误码：§58 的前端
 * 本地码表里与麦克风有关的**只有** `MIC_PERMISSION_DENIED` 一条，它是冻结契约的
 * 一部分。其余三种失败（设备被占用、没有设备、浏览器不支持）在契约里没有码，
 * 硬塞进去就等于单方面改契约；它们在 app 内部用类别表达，文案层负责给出下一步动作。
 */
export type MicFailureKind =
  | 'permission-denied'
  | 'device-busy'
  | 'device-missing'
  | 'unsupported'
  /** 轨道在开启之后结束了（设备被拔出、被系统或其它程序抢走）。 */
  | 'track-ended'

export interface MicFailure {
  kind: MicFailureKind
  /** §58 的错误码；只有权限被拒那一档有值（与摄像头完全同构）。 */
  code: 'MIC_PERMISSION_DENIED' | null
  /** 可直接展示给学生的中文（包含"下一步做什么"，且明说不会影响上课）。 */
  message: string
}

/**
 * 每类失败的文案。
 *
 * 三条约束（与 `camera-capture.ts` 一致）：
 * 1. 不出现英文技术信息、不出现 error.name、不出现原始报文（§58）；
 * 2. 必须给出下一步动作（去权限设置 / 关掉占用程序 / 换浏览器）；
 * 3. **必须明说"不影响上课"**。麦克风是 optional（§25），而老师发起的语音沟通
 *    会让学生以为"不开麦就上不了课"——那句话会让他直接放弃这节课。
 *    同时要说清"老师仍能单向讲话"（§25 的原文），否则学生会以为拒绝=整段沟通结束。
 */
const MIC_FAILURE_MESSAGES: Record<MicFailureKind, string> = {
  'permission-denied':
    '浏览器没有授予麦克风权限。你仍然可以正常上课，也仍然能听到老师讲话。如需开启，请点击地址栏左侧的权限图标允许麦克风后重试。',
  'device-busy':
    '麦克风可能正被其他程序占用（例如会议软件）。关闭占用麦克风的程序后可以重试，这不会影响你上课。',
  'device-missing': '没有检测到可用的麦克风。你仍然可以正常上课，也仍然能听到老师讲话。',
  unsupported: '当前浏览器不支持麦克风采集。请改用最新版 Chrome 或 Edge；这仍然不影响你上课。',
  'track-ended': '麦克风已经停止（设备被拔出或被其他程序占用）。你可以重新开启，这不会影响你上课。',
}

export function describeMicFailure(kind: MicFailureKind): string {
  return MIC_FAILURE_MESSAGES[kind]
}

/** 把类别折成 {@link MicFailure}（码只在这一处判定，避免多处各写一遍）。 */
export function toMicFailure(kind: MicFailureKind): MicFailure {
  return {
    kind,
    code: kind === 'permission-denied' ? 'MIC_PERMISSION_DENIED' : null,
    message: describeMicFailure(kind),
  }
}

/**
 * 浏览器抛出的 `error.name` → 类别（与摄像头的映射同一套判断）。
 *
 * 顺序有意义：`NotAllowedError` 必须先判——macOS 上"系统隐私设置里没勾选浏览器"
 * 与"学生点了拒绝"都归到它，对学生的下一步动作是同一件事（去权限设置）。
 * `NotReadableError` 是"授权通过了但设备打不开"（最常见成因是别的程序占着麦克风），
 * 报成权限问题会让学生反复去点权限弹窗却永远失败。
 */
export function toMicFailureKind(cause: unknown): MicFailureKind {
  const name = cause instanceof Error ? cause.name : null
  switch (name) {
    case 'NotAllowedError':
    case 'SecurityError':
      return 'permission-denied'
    case 'NotReadableError':
    case 'TrackStartError':
    case 'AbortError':
      return 'device-busy'
    case 'NotFoundError':
    case 'DevicesNotFoundError':
    case 'OverconstrainedError':
      return 'device-missing'
    default:
      return 'unsupported'
  }
}

/** 能力自检的失败原因（发生在请求之前，与上面的运行期失败分开）。 */
export type MicrophoneUnsupportedReason = 'no-mediaDevices' | 'no-getUserMedia'

export interface MicrophoneSupport {
  supported: boolean
  reason?: MicrophoneUnsupportedReason
}

/**
 * §17 的能力自检在麦克风这一侧的对应物：**纯读取检查，无任何副作用**。
 *
 * 浏览器根本没有 `getUserMedia` 时必须提前拒绝，否则学生会看到浏览器自己的英文报错。
 * 这也保证了会话页在挂载时**永远不会**因为读这个函数而弹出麦克风授权框
 * （§25：只有学生点击才请求）。
 */
export function checkMicrophoneSupport(): MicrophoneSupport {
  const devices: MediaDevices | undefined = navigator.mediaDevices
  if (!devices) return { supported: false, reason: 'no-mediaDevices' }
  if (typeof devices.getUserMedia !== 'function')
    return { supported: false, reason: 'no-getUserMedia' }
  return { supported: true }
}

/**
 * 请求麦克风的约束（§25 冻结的那一行：`getUserMedia({ audio: true })`）。
 *
 * WHY 只有 `audio` 而没有 `video: false`：
 * - `video` 缺省就是不要视频，写不写都不影响结果；
 * - 但显式写 `video: true`（哪怕本意是"顺手带上"）会弹出**摄像头**授权，
 *   而摄像头有它自己的开关（§24）。麦克风这条路多要一次授权就是超范围收集。
 * 保持与任务书逐字一致，也让"学生端到底申请了什么"在代码里一眼可查。
 */
export const MIC_CONSTRAINTS: MediaStreamConstraints = { audio: true }

/**
 * 一条已经拿到授权的麦克风轨道。
 *
 * 与 `CameraCapture` / `ScreenCapture` 同形（`stream` / `track` / `stop` / `onEnded`），
 * 让会话 store 处理三条轨道的代码保持同一种写法；差别只在语义：
 * 屏幕轨道进课堂前必须存在（§21），摄像头与麦克风随时可以没有（§21/§25）。
 */
export interface MicrophoneCapture {
  stream: MediaStream
  track: MediaStreamTrack
  /** 幂等：重复调用不会报错。释放设备（系统麦克风指示灯必须灭）的唯一出口。 */
  stop(): void
  /** 订阅"轨道结束"；返回取消订阅函数（与 `CameraCapture.onEnded` 同约定）。 */
  onEnded(listener: () => void): () => void
}

/** 幂等停掉一批轨道：某一条抛错不能导致后面的留在 live 状态（指示灯还亮着）。 */
function stopAllTracks(tracks: readonly MediaStreamTrack[]): void {
  for (const track of tracks) {
    try {
      track.stop()
    } catch {
      // 停不掉也没有别的补救手段；绝不能因此让上层跳过其余轨道。
    }
  }
}

/**
 * 尽力停掉"半成品"流（`getUserMedia` 失败时浏览器仍可能已经创建了流）。
 *
 * 麦克风比摄像头更需要这一点：一条活着的 audio track 意味着系统级的录音指示灯
 * 一直亮着，而学生会以为自己正在被监听。
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
 * `getUserMedia({ audio: true })` + 错误归类（§25）。
 *
 * **所有**失败路径都保证已经把这批轨道全部 stop 掉——麦克风这一侧的代价是
 * "学生看到开启失败、设备却仍在采集"。
 */
export async function requestMicrophone(): Promise<MicrophoneCapture> {
  const support = checkMicrophoneSupport()
  if (!support.supported) {
    // 连 API 都没有：绝不发起请求（那只会得到浏览器自己的英文报错）。
    throw new MicrophoneCaptureError('unsupported', { causeName: support.reason ?? null })
  }

  let stream: MediaStream
  try {
    stream = await navigator.mediaDevices.getUserMedia(MIC_CONSTRAINTS)
  } catch (cause) {
    stopEverything(cause)
    throw new MicrophoneCaptureError(toMicFailureKind(cause), {
      causeName: cause instanceof Error ? cause.name : null,
      cause,
    })
  }

  const tracks = stream.getTracks()
  const track = stream.getAudioTracks()[0]
  if (!track) {
    // 拿到了流却没有 audio track：当成"设备不可用"，并把这批轨道全部释放。
    stopAllTracks(tracks)
    throw new MicrophoneCaptureError('device-missing', { causeName: 'no-audio-track' })
  }

  let stopped = false
  let unsubscribeEnded: (() => void) | null = null

  const stop = (): void => {
    // 幂等：先摘监听再停轨道，避免 track.stop() 与 ended 互相触发形成回声。
    unsubscribeEnded?.()
    unsubscribeEnded = null
    if (stopped) return
    stopped = true
    stopAllTracks(tracks)
  }

  return {
    stream,
    track,
    stop,
    onEnded(listener) {
      // 已经结束的轨道不会再触发 ended：直接回调一次，否则界面会停在"麦克风已开启"。
      if (stopped || track.readyState === 'ended') {
        listener()
        return () => undefined
      }

      const handleEnded = (): void => {
        // 设备没了就必须真的释放：不 stop 的话浏览器会一直认为这条轨道还活着。
        stop()
        listener()
      }

      track.addEventListener('ended', handleEnded)
      /**
       * 两种形态都挂，理由与 `camera-capture.ts` 完全相同：`ended` 事件由浏览器
       * 决定走哪条派发路径（真实 Chrome 两条都会走，happy-dom 这类替身只支持其一）。
       */
      track.onended = handleEnded

      unsubscribeEnded = () => {
        track.removeEventListener('ended', handleEnded)
        if (track.onended === handleEnded) track.onended = null
      }
      return unsubscribeEnded
    },
  }
}

/** 释放采集（幂等，对 null / undefined 安全）。 */
export function releaseMicrophoneCapture(capture: MicrophoneCapture | null | undefined): void {
  if (!capture) return
  capture.stop()
}

/**
 * 本地静音（§31 的"静音自己"）。
 *
 * WHY 用 `track.enabled` 而不是 `stop()` / `unpublish`：
 * - `stop()` 会释放设备、熄灭系统麦克风指示灯，学生就**再也听不到老师讲话之外**
 *   也说不了话，必须重新走一次 getUserMedia 才能回来——那是一次真正的"挂断"，
 *   而 §31 要的是一个可逆的自我静音；
 * - `enabled = false` 让浏览器继续发送一条静音轨道（RTP 仍在，只是内容为静音），
 *   老师的下行订阅不会因此被拆掉，取消静音是瞬时生效的。
 *
 * 返回是否真的作用在了一条轨道上（没有麦克风时这个动作本身就是空操作）。
 */
export function applyMicrophoneMute(
  capture: MicrophoneCapture | null | undefined,
  muted: boolean,
): boolean {
  if (!capture) return false
  capture.track.enabled = !muted
  return true
}

/**
 * 采集层的失败。
 *
 * 刻意**不**复用 `CameraCaptureError`：那个类的语义是摄像头那一路的失败，
 * 复用它会让"麦克风权限被拒"在日志里长成"摄像头"，也会让文案选择失去依据。
 */
export class MicrophoneCaptureError extends Error {
  readonly kind: MicFailureKind
  /** 浏览器原始 `error.name`（排障用；只出现在日志的错误名里，不进界面）。 */
  readonly causeName: string | null

  constructor(
    kind: MicFailureKind,
    options: { causeName?: string | null; message?: string; cause?: unknown } = {},
  ) {
    super(
      options.message ?? kind,
      options.cause === undefined ? undefined : { cause: options.cause },
    )
    this.name = 'MicrophoneCaptureError'
    this.kind = kind
    this.causeName = options.causeName ?? null
  }
}

/** 把任意异常折成 {@link MicFailureKind}（不认识的一律按"浏览器不支持"处理）。 */
export function toMicFailureKindOf(cause: unknown): MicFailureKind {
  return cause instanceof MicrophoneCaptureError ? cause.kind : toMicFailureKind(cause)
}
