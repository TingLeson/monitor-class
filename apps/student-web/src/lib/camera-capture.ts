/**
 * 摄像头采集（§24 / §58 的前端本地错误码 / §75）。
 *
 * 与 `screen-capture.ts` 是**刻意分开**的两份，而且这个文件里没有、也永远不该有
 * `getDisplayMedia`。两条理由，都是业务不变量而不是代码整洁问题：
 *
 * 1. **屏幕是 mandatory，摄像头是 optional**（§21）。屏幕那条轨道被 §16 的 Gate
 *    层层加码（displaySurface 必须是 monitor），摄像头只需要一个普通的
 *    `getUserMedia({ video: true })`。把两者写在一起，早晚会有人给摄像头也套上 Gate，
 *    或者更糟——让摄像头的失败把学生挡在课堂外面。
 * 2. **不能合并授权**。§24 要求"进入课堂之后才显示开启摄像头"，也就是摄像头权限
 *    只能在学生主动点击时请求。与屏幕捕获合并成一次请求，等于在 PreJoin 阶段就
 *    弹出摄像头授权框，那正是 §24 明令禁止的时序。
 *
 * 这个文件只负责"拿到一条 video track / 把浏览器错误翻成可执行的下一步"，
 * 状态机（off / requesting / on / error）与 publish 在 `stores/media-session.ts`：
 * 状态是**会话**的属性（它要跨视图存活、要在离开课堂时统一释放），不是采集的属性。
 */

/**
 * 摄像头状态机（§24 / §56）。
 *
 * ```
 *        enableCamera()               拿到轨道 + publish 成功
 *  off ─────────────────→ requesting ─────────────────────→ on
 *   ↑                          │                              │
 *   │                    权限被拒 / 设备占用 /             disableCamera()
 *   │                    浏览器不支持 / 轨道结束                │
 *   │                          ↓                              │
 *   └──── enableCamera() ──── error ←─────────────────────────┘
 * ```
 *
 * WHY 用四个状态而不是一个布尔：`requesting` 必须与 `off` 分开，否则"点一下没反应"
 * 的那几百毫秒里按钮是活的，学生会连点，而每一次点击都是一次新的设备请求；
 * `error` 必须与 `off` 分开，因为"没开"和"打不开"要给完全不同的话（后者要给出
 * 下一步动作）。摄像头**不属于**课堂阶段（`MediaSessionPhase`）——
 * 它开或关都不会让会话变成 ONLINE / SCREEN_LOST（§21/§24）。
 */
export const CAMERA_STATES = ['off', 'requesting', 'on', 'error'] as const

export type CameraState = (typeof CAMERA_STATES)[number]

/**
 * 摄像头错误类别。
 *
 * WHY 复用 §58 的 `CAMERA_PERMISSION_DENIED` 而不是自造一套完整错误码：
 * §58 的前端本地码表里与摄像头有关的**只有** `CAMERA_PERMISSION_DENIED` 一条，
 * 它是冻结契约的一部分。其余三种失败（设备被占用、没有设备、浏览器不支持）
 * 在契约里没有码，硬塞进去就等于单方面改契约；它们在 app 内部用类别表达，
 * 文案层负责给出下一步动作。
 */
export type CameraFailureKind =
  | 'permission-denied'
  | 'device-busy'
  | 'device-missing'
  | 'unsupported'
  /** 轨道在开启之后结束了（设备被拔出、被系统或其它程序抢走）。 */
  | 'track-ended'

export interface CameraFailure {
  kind: CameraFailureKind
  /**
   * §58 的错误码；只有权限被拒那一档有值。
   *
   * 保留它是因为"这是权限问题"与"这是设备问题"在排障时是两类完全不同的记录，
   * 而码是契约里唯一稳定的标识。没有码的类别用 `null` 明确表达"契约里没有这一档"，
   * 而不是借一个语义不符的码来凑。
   */
  code: 'CAMERA_PERMISSION_DENIED' | null
  /** 可直接展示给学生的中文（包含"下一步做什么"，且明说不会影响上课）。 */
  message: string
}

/**
 * 每类失败的文案。
 *
 * 三条约束（与 `screen-capture-messages.ts` 一致）：
 * 1. 不出现英文技术信息、不出现 error.name、不出现原始报文（§58）；
 * 2. 必须给出下一步动作（去权限设置 / 关掉占用程序 / 换浏览器）；
 * 3. **必须明说"不影响上课"**。摄像头是 optional（§21），学生最可能的误解是
 *    "摄像头打不开就上不了课了"——那句话会让他直接放弃这节课。
 */
const CAMERA_FAILURE_MESSAGES: Record<CameraFailureKind, string> = {
  'permission-denied':
    '浏览器没有授予摄像头权限，你仍然可以正常上课。如需开启，请点击地址栏左侧的权限图标允许摄像头后重试。',
  'device-busy':
    '摄像头可能正被其他程序占用（例如会议软件）。关闭占用摄像头的程序后可以重试，这不会影响你上课。',
  'device-missing': '没有检测到可用的摄像头。你仍然可以正常上课。',
  unsupported: '当前浏览器不支持摄像头采集。请改用最新版 Chrome 或 Edge；这仍然不影响你上课。',
  'track-ended': '摄像头已经停止（设备被拔出或被其他程序占用）。你可以重新开启，这不会影响你上课。',
}

export function describeCameraFailure(kind: CameraFailureKind): string {
  return CAMERA_FAILURE_MESSAGES[kind]
}

/** 把类别折成 {@link CameraFailure}（码只在这一处判定，避免多处各写一遍）。 */
export function toCameraFailure(kind: CameraFailureKind): CameraFailure {
  return {
    kind,
    code: kind === 'permission-denied' ? 'CAMERA_PERMISSION_DENIED' : null,
    message: describeCameraFailure(kind),
  }
}

/**
 * 浏览器抛出的 `error.name` → 类别。
 *
 * 映射顺序有意义：`NotAllowedError` 必须先判——macOS 上"系统隐私设置里没勾选浏览器"
 * 与"学生点了拒绝"都归到它，而它们对学生的下一步动作是同一件事（去权限设置）。
 *
 * `NotReadableError` / `TrackStartError` 是"授权通过了但设备打不开"，最常见的成因
 * 就是另一个程序正占着摄像头。它**不是**权限问题，报成权限问题会让学生反复去点
 * 权限弹窗却永远失败。`AbortError` 语义上是"这一次没成"，归到设备占用一侧
 * （学生重新点一次通常就好），而不是让界面显示一个吓人的"浏览器不支持"。
 */
export function toCameraFailureKind(cause: unknown): CameraFailureKind {
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
export type CameraUnsupportedReason = 'no-mediaDevices' | 'no-getUserMedia'

export interface CameraSupport {
  supported: boolean
  reason?: CameraUnsupportedReason
}

/**
 * §17 的能力自检在摄像头这一侧的对应物：**纯读取检查，无任何副作用**。
 *
 * 不做服务端式的"试一下再说"：浏览器根本没有 `getUserMedia` 时必须提前拒绝，
 * 否则学生会看到浏览器自己弹出的英文报错，而我们要给的是"换 Chrome / Edge"。
 * 这也保证了 PreJoin 与课堂页在挂载时**永远不会**因为读这个函数而产生授权弹窗。
 */
export function checkCameraSupport(): CameraSupport {
  const devices: MediaDevices | undefined = navigator.mediaDevices
  if (!devices) return { supported: false, reason: 'no-mediaDevices' }
  if (typeof devices.getUserMedia !== 'function')
    return { supported: false, reason: 'no-getUserMedia' }
  return { supported: true }
}

/**
 * 请求摄像头的约束（§24 冻结的那一行：`getUserMedia({ video: true })`）。
 *
 * WHY 只有 `video` 而没有 `audio: false`：
 * - `audio` 缺省就是不要音频，写不写都不影响结果；
 * - 但显式写 `audio: true`（哪怕本意是"顺手带上"）会弹出**麦克风**授权，
 *   而麦克风属于 Phase 10（§25/§31），这里多要一次授权就是超范围收集。
 * 保持与任务书逐字一致，也让"学生端到底申请了什么"在代码里一眼可查。
 */
export const CAMERA_CONSTRAINTS: MediaStreamConstraints = { video: true }

/**
 * 一条已经拿到授权的摄像头轨道。
 *
 * 与 `ScreenCapture` 同形（`stream` / `track` / `stop` / `onEnded`），
 * 让会话 store 处理两条轨道的代码保持同一种写法；差别只在语义：
 * 屏幕轨道进课堂前必须存在（§21），摄像头轨道随时可以没有。
 */
export interface CameraCapture {
  stream: MediaStream
  track: MediaStreamTrack
  /** 幂等：重复调用不会报错。释放设备（摄像头指示灯必须灭）的唯一出口。 */
  stop(): void
  /** 订阅"轨道结束"；返回取消订阅函数（与 `ScreenCapture.onEnded` 同约定）。 */
  onEnded(listener: () => void): () => void
}

/** 幂等停掉一批轨道：某一条抛错不能导致后面的留在 live 状态（灯还亮着）。 */
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
 * 尽力停掉"半成品"流。
 *
 * `getUserMedia` 失败时理论上不返回流，但浏览器实现里"已创建 MediaStream 再报错"
 * 并非不可能。摄像头比屏幕更需要在这一点上保守：只要有一条 video track 还活着，
 * 系统级的摄像头指示灯就亮着，学生会以为自己正被看着。
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
 * `getUserMedia({ video: true })` + 错误归类（§24）。
 *
 * **所有**失败路径都保证已经把这批轨道全部 stop 掉——摄像头这一侧的代价特别直观：
 * 漏掉一条就等于学生在"开启失败"的提示下继续亮着摄像头灯。
 */
export async function requestCamera(): Promise<CameraCapture> {
  const support = checkCameraSupport()
  if (!support.supported) {
    // 连 API 都没有：绝不发起请求（那只会得到浏览器自己的英文报错）。
    throw new CameraCaptureError('unsupported', { causeName: support.reason ?? null })
  }

  let stream: MediaStream
  try {
    stream = await navigator.mediaDevices.getUserMedia(CAMERA_CONSTRAINTS)
  } catch (cause) {
    stopEverything(cause)
    throw new CameraCaptureError(toCameraFailureKind(cause), {
      causeName: cause instanceof Error ? cause.name : null,
      cause,
    })
  }

  const tracks = stream.getTracks()
  const track = stream.getVideoTracks()[0]
  if (!track) {
    // 拿到了流却没有 video track：当成"设备不可用"，并把这批轨道全部释放。
    stopAllTracks(tracks)
    throw new CameraCaptureError('device-missing', { causeName: 'no-video-track' })
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
      // 已经结束的轨道不会再触发 ended：直接回调一次，否则界面会停在"摄像头已开启"。
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
       * 两种形态都挂，理由与 `screen-capture.ts` 完全相同：`ended` 事件由浏览器
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
export function releaseCameraCapture(capture: CameraCapture | null | undefined): void {
  if (!capture) return
  capture.stop()
}

/**
 * 采集层的失败。
 *
 * 刻意**不**复用 `ScreenGateError`：那个类的语义是"Gate 拒绝了这次共享"
 * （§16 的硬闸门），而摄像头根本没有 Gate。复用它会让调用方误以为摄像头也有
 * 一条"必须整屏"的规则，从而在文案里写出"请重新选择摄像头"这种无意义的话。
 */
export class CameraCaptureError extends Error {
  readonly kind: CameraFailureKind
  /** 浏览器原始 `error.name`（排障用；只出现在日志的错误名里，不进界面）。 */
  readonly causeName: string | null

  constructor(
    kind: CameraFailureKind,
    options: { causeName?: string | null; message?: string; cause?: unknown } = {},
  ) {
    super(
      options.message ?? kind,
      options.cause === undefined ? undefined : { cause: options.cause },
    )
    this.name = 'CameraCaptureError'
    this.kind = kind
    this.causeName = options.causeName ?? null
  }
}

/** 把任意异常折成 {@link CameraFailureKind}（不认识的一律按"浏览器不支持"处理）。 */
export function toCameraFailureKindOf(cause: unknown): CameraFailureKind {
  return cause instanceof CameraCaptureError ? cause.kind : toCameraFailureKind(cause)
}
