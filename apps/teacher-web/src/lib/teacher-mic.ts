/**
 * 老师自己的麦克风采集（§27 / §31 / §76）。
 *
 * 与学生端的 `mic-capture.ts` 是**刻意分开**的两份（与本目录下 media 的分裂同一套
 * 理由：两个 app 的权限模型与失败模式完全不同）。老师这一侧小得多，因为：
 *
 * 1. **没有"请求提示"要回应**。老师麦克风只有两种触发：老师点开关，或者他在
 *    Focus 面板上点了「语音沟通」而被后端以 `TEACHER_MIC_REQUIRED` 拒绝后点
 *    "开启麦克风"。两者都是一次明确的用户点击——不存在"某条事件到达时该不该问"的问题。
 * 2. **没有自我静音**。§31 的"静音自己"是学生侧的入口（老师这边结束沟通就是
 *    「结束语音」整个动作），所以这里没有 `track.enabled` 这条路径。
 * 3. **失败只有一个去处**：Focus 面板上的一句话 + 一个重试按钮。没有"不影响上课"
 *    这种安慰——老师开不了麦只影响他能不能讲话，而课堂监督本身完全不依赖它。
 *
 * 与采集层同样的纪律：`stop()` 是唯一能让系统麦克风指示灯灭掉的动作，
 * 而且必须在"关麦"与"离开监督墙"两条路径上都被调用。
 */

export type TeacherMicFailureKind =
  'permission-denied' | 'device-busy' | 'device-missing' | 'unsupported' | 'track-ended'

/** 老师麦克风的失败文案：只说"发生了什么 + 下一步做什么"，不解释课堂状态。 */
const TEACHER_MIC_FAILURE_MESSAGES: Record<TeacherMicFailureKind, string> = {
  'permission-denied': '浏览器没有授予麦克风权限。请点击地址栏左侧的权限图标允许麦克风后重试。',
  'device-busy': '麦克风可能正被其他程序占用。关闭占用麦克风的程序后可以重试。',
  'device-missing': '没有检测到可用的麦克风。请接入麦克风后重试。',
  unsupported: '当前浏览器不支持麦克风采集。请改用最新版 Chrome 或 Edge。',
  'track-ended': '麦克风已经停止（设备被拔出或被其他程序占用）。你可以重新开启。',
}

export function describeTeacherMicFailure(kind: TeacherMicFailureKind): string {
  return TEACHER_MIC_FAILURE_MESSAGES[kind]
}

/** 老师麦克风状态机（与学生端同形，但只存在于监督 store 里）。 */
export type TeacherMicState = 'off' | 'requesting' | 'on' | 'error'

export interface TeacherMicrophoneCapture {
  stream: MediaStream
  track: MediaStreamTrack
  /** 幂等：释放设备（麦克风指示灯必须灭）的唯一出口。 */
  stop(): void
  /** 订阅"轨道结束"；返回取消订阅函数。 */
  onEnded(listener: () => void): () => void
}

/**
 * 只认 `audio`：老师 Token 的 `canPublishSources` 里**只有** microphone（§27），
 * 多要一次摄像头授权既无用途，也会让"老师端到底申请了什么"变得需要解释。
 */
export const TEACHER_MIC_CONSTRAINTS: MediaStreamConstraints = { audio: true }

function stopAllTracks(tracks: readonly MediaStreamTrack[]): void {
  for (const track of tracks) {
    try {
      track.stop()
    } catch {
      // 停不掉也没有别的补救手段；绝不能因此让上层跳过其余轨道。
    }
  }
}

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

/** 浏览器错误名 → 类别（与学生端同一套判断，理由见那边的注释）。 */
export function toTeacherMicFailureKind(cause: unknown): TeacherMicFailureKind {
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

/**
 * `getUserMedia({ audio: true })` + 错误归类。
 *
 * **所有**失败路径都保证已经把这批轨道全部 stop 掉——老师这一侧同样不能出现
 * "提示说失败、系统麦克风指示灯还亮着"。
 */
export async function requestTeacherMicrophone(): Promise<TeacherMicrophoneCapture> {
  const devices: MediaDevices | undefined = navigator.mediaDevices
  if (!devices || typeof devices.getUserMedia !== 'function') {
    throw new TeacherMicCaptureError('unsupported')
  }

  let stream: MediaStream
  try {
    stream = await devices.getUserMedia(TEACHER_MIC_CONSTRAINTS)
  } catch (cause) {
    stopEverything(cause)
    throw new TeacherMicCaptureError(toTeacherMicFailureKind(cause), { cause })
  }

  const tracks = stream.getTracks()
  const track = stream.getAudioTracks()[0]
  if (!track) {
    stopAllTracks(tracks)
    throw new TeacherMicCaptureError('device-missing')
  }

  let stopped = false
  let unsubscribeEnded: (() => void) | null = null

  const stop = (): void => {
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
      if (stopped || track.readyState === 'ended') {
        listener()
        return () => undefined
      }
      const handleEnded = (): void => {
        stop()
        listener()
      }
      track.addEventListener('ended', handleEnded)
      // 两种形态都挂：真实 Chrome 两条都走，测试替身只支持其一（同学生端）。
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
export function releaseTeacherMicrophone(
  capture: TeacherMicrophoneCapture | null | undefined,
): void {
  if (!capture) return
  capture.stop()
}

export class TeacherMicCaptureError extends Error {
  readonly kind: TeacherMicFailureKind

  constructor(kind: TeacherMicFailureKind, options: { cause?: unknown } = {}) {
    super(kind, options.cause === undefined ? undefined : { cause: options.cause })
    this.name = 'TeacherMicCaptureError'
    this.kind = kind
  }
}

/** 把任意异常折成类别（不认识的一律按"浏览器不支持"处理）。 */
export function toTeacherMicFailureKindOf(cause: unknown): TeacherMicFailureKind {
  return cause instanceof TeacherMicCaptureError ? cause.kind : toTeacherMicFailureKind(cause)
}
