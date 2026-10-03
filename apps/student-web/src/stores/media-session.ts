import type { RealtimeConnectionState } from '@classwatch/api-client'
import type { RealtimeEvent, StudentClassroom } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, markRaw, ref, shallowRef } from 'vue'
import {
  releaseCameraCapture,
  requestCamera,
  toCameraFailure,
  toCameraFailureKindOf,
  type CameraCapture,
  type CameraFailure,
  type CameraState,
} from '../lib/camera-capture.ts'
import {
  applyMicrophoneMute,
  releaseMicrophoneCapture,
  requestMicrophone,
  toMicFailure,
  toMicFailureKindOf,
  type MicFailure,
  type MicState,
  type MicrophoneCapture,
} from '../lib/mic-capture.ts'
import {
  applyTalkRequest,
  applyTalkStarted,
  declineTalkRequest,
  endPrivateTalk,
  hasPrivateTalk,
  markMicrophoneEnabled,
  privateTalkActiveText,
  privateTalkRequestTitle,
  type StudentPrivateTalk,
} from '../lib/private-talk.ts'
import {
  createScreenPublisherRoom,
  type ConnectionQualityLevel,
  type MediaCredentials,
  type ScreenPublisherRoom,
  type TeacherAudioState,
} from '../lib/media/media-room.ts'
import {
  classroomFallbackPollMs,
  describeMediaFailure,
  type MediaFailure,
  type MediaFailureKind,
  type MediaSessionPhase,
} from '../lib/media-session-state.ts'
import {
  describeScreenCapture,
  releaseScreenCapture,
  toJoinCaptureDiagnostics,
  type ScreenCapture,
} from '../lib/screen-capture.ts'
import { getClassroom } from '../lib/student-classrooms-api.ts'
import { joinClassroom, leaveSession } from '../lib/student-sessions-api.ts'

/**
 * `prepare()` 的入参：PreJoin 在 join 成功之后交接过来的一切。
 *
 * `capture` 是 Phase 5 Gate 的产物（§20 要求复用的那一条轨道），
 * `credentials` 是后端签发的短时凭据（§43/§44）。
 */
export interface PrepareInput {
  sessionId: string
  classroomId: string
  credentials: MediaCredentials
  capture: ScreenCapture
}

/**
 * 课堂会话 store（§20 / §21 / §22 / §28 / §43 / §49 / §56）。
 *
 * 它负责的是"从 PreJoin 拿到凭据之后，到离开课堂为止"的整条生命周期：
 *
 * ```
 * prepare()             begin()                        track ended (§22)
 * PreJoin ──────────→ prepared ──→ connecting ──→ online ──────────→ screen-lost
 *                     │                │                                  │
 *                     │          连接/publish 失败                 publishCapture()
 *                     │                ↓                                  │
 *                     │           media-error ── retry() ── join 新 token │
 *                     │                                                   │
 *                     └────────────── leave() / 课堂被关闭(§49) ──────────┘
 *                                        ↓
 *                                  left / closed
 * ```
 *
 * 四条贯穿全文件的决定：
 *
 * 1. **凭据（token / livekitUrl）只在这个 store 的闭包里**，既不进响应式 state，
 *    也不进 URL / Web Storage / 日志（§44）。放进 `ref` 就等于让 devtools、
 *    SSR 快照、任何一次 `JSON.stringify(store.$state)` 都有机会把它带出去；
 *    而闭包变量连"不小心渲染出来"都不可能。
 * 2. **屏幕轨道只有一条，且复用 Phase 5 Gate 的产物**（§20）。本 store 从不调用
 *    `getDisplayMedia`，重新共享走的是"让 screen-share store 再过一次 Gate"这条
 *    唯一路径（见 SessionView），因此学生不会看到第二次授权弹窗。
 * 3. **轨道丢失绝不 republish 同一条轨道**（§22）。`ended` 之后那条 track 已经死了，
 *    再 publish 只会得到一个静默失败或一条永远没有画面的轨道；正确做法是进入
 *    `screen-lost` 提示态，等学生重新共享一条**新**轨道。
 * 4. **服务端权威、客户端快速反馈**（§46）。`track.onended` 让界面**立刻**反应，
 *    而 `SCREEN_LOST` / `SCREEN_RESTORED` 事件才是"老师那边到底看到了什么"的答案。
 *    两者都写进下面这一份状态：服务端说丢了，本地即便还握着一条轨道也不再显示
 *    "正在共享"（见 `handleServerScreenLost`）；学生重新共享成功后，界面停在
 *    "等待课堂确认"，直到服务端说 `SCREEN_RESTORED` 才算恢复正常。
 * 5. **课堂是否开着以 WebSocket 事件为主、低频轮询兜底**（§47/§49）：
 *    事件负责快，轮询负责"通道断了也不漏"（见 `syncPollTimer`）。
 * 6. **摄像头与麦克风是可选设备，与课堂状态完全解耦**（§21/§24/§25）。`cameraState` /
 *    `micState` 只回答"这个设备开没开"，它们不是 `phase` 的一部分：设备打不开不会把
 *    phase 变成 media-error（学生照样能上课），屏幕丢了也不会因为"麦克风还开着"就显得
 *    正常（SessionView 里那条 ⚠ 提示是独立的）。`on` 的含义被收紧成"已发布、老师看得见 /
 *    听得到"——连接一断就释放设备，绝不留下亮着灯却无人能听/看的设备。
 * 7. **私密语音的状态只由服务端事件驱动**（§25/§31）。学生端没有"发起/结束语音沟通"
 *    的入口：老师选择学生、老师麦克风的订阅权限都在服务端。学生会话页能做的只有
 *    三件事——把提示显示出来、在学生**点击**之后请求麦克风（§25 明令禁止自动开麦）、
 *    以及如实显示老师麦克风的音频到底能不能出声（自动播放策略可能拦住它）。
 */
export const useMediaSessionStore = defineStore('student-media-session', () => {
  /* ---------------------------------------------------------------------- */
  /* 可被界面读取的状态                                                      */
  /* ---------------------------------------------------------------------- */

  const phase = ref<MediaSessionPhase>('idle')
  /** 当前会话 id；join / retry 之后可能变化（§50：后端可以取代旧连接）。 */
  const sessionId = ref<string | null>(null)
  /** 课堂详情（名称 + 状态）。轮询与事件都会刷新它，§49 的"老师已关闭"就来自这里。 */
  const classroom = ref<StudentClassroom | null>(null)
  const quality = ref<ConnectionQualityLevel>('unknown')
  const failure = ref<MediaFailure | null>(null)
  /** 客户端是否正在等待连接完成（用于按钮 loading，不参与状态判断）。 */
  const busy = ref(false)
  /** LiveKit 正在自动重连（§52）；界面据此显示"正在重连"，不改 phase。 */
  const reconnecting = ref(false)
  /**
   * 服务端是否已经判定"屏幕丢失"、并且还没收到 `SCREEN_RESTORED`（§46）。
   *
   * 与 `phase === 'screen-lost'` **不是**同一件事：学生重新共享成功后 phase 会回到
   * online，但服务端还没确认；这段时间界面必须说"等待课堂确认"，而不是
   * 一口咬定"正在共享整个屏幕"——老师那边可能什么都没收到。
   */
  const screenLostByServer = ref(false)
  /** 实时通道状态，只用来决定兜底轮询的频率（见 syncPollTimer）。 */
  const realtimeState = ref<RealtimeConnectionState>('closed')

  /* ---------------------------------------------------------------------- */
  /* 摄像头（§24）                                                           */
  /* ---------------------------------------------------------------------- */

  /**
   * 摄像头状态机（`off | requesting | on | error`，定义与迁移图见 camera-capture.ts）。
   *
   * 它与 `phase` 是两件**互不影响**的事：这里是"这个可选设备现在开没开"，
   * `phase` 是"我这一端在课堂里的位置"。把摄像头塞进 phase 会立刻产生两个错误结论：
   * 摄像头失败变成"媒体连接失败"（学生被吓到，其实课堂好好的），
   * 以及"摄像头开着"掩盖掉"屏幕已经停了"（§21 的强制规则被绕过）。
   */
  const cameraState = ref<CameraState>('off')
  /** 摄像头失败的类别 + 中文文案（失败不影响会话，见文件头第 6 点）。 */
  const cameraFailure = ref<CameraFailure | null>(null)
  /**
   * 供 §56 的小自视画面绑定的本地流。
   *
   * WHY 放 `shallowRef` + `markRaw`：MediaStream 是浏览器宿主对象，被 Vue 深度代理
   * 之后 `srcObject` 赋值会出现 `Illegal invocation`（同 screen-share store 里那段
   * 关于 markRaw 的说明）。视图只把它交给 `<video>.srcObject`，不做任何业务判断。
   *
   * WHY 允许这一处 `<video>`，而屏幕上仍然禁止预览（§56）：屏幕预览会形成
   * screen-inside-screen-inside-screen，而且"我正在共享整屏"这句话本来就由状态行回答；
   * 摄像头则相反——学生**必须**能确认自己真的在画面里（角度、光线、有没有开错设备），
   * 而那句话是任何文字都代替不了的。两者是不同的问题，因此规则不同。
   */
  const cameraStream = shallowRef<MediaStream | null>(null)

  /* ---------------------------------------------------------------------- */
  /* 麦克风（§25）                                                           */
  /* ---------------------------------------------------------------------- */

  /**
   * 麦克风状态机（`off | requesting | on | error`，定义与迁移图见 mic-capture.ts）。
   *
   * 与 `phase` 是两件**互不影响**的事，理由与摄像头完全一致（见上面 cameraState 的
   * 说明）：麦克风打不开只是"这个可选设备没开"，不是"媒体连接失败"；
   * 反过来，麦克风开着也不代表屏幕还在共享（§21 的强制规则不受任何可选设备影响）。
   *
   * 它与摄像头**各自独立**：学生可以只开麦、也可以只开摄像头，两者互不牵连。
   */
  const micState = ref<MicState>('off')
  /** 麦克风失败的类别 + 中文文案（失败不影响会话，见 mic-capture.ts 的文案表）。 */
  const micFailure = ref<MicFailure | null>(null)
  /**
   * 本地自我静音（§31 的"静音自己"）。
   *
   * WHY 单独一个布尔而不是把 `micState` 改成 `muted`：静音时设备仍然开着、
   * 轨道仍然在发布（老师那端的订阅不会被拆掉），只是声音变成了静音。
   * 混进状态机会让"未开启"和"已静音"变成一个状态，界面就无法同时说清
   * "麦克风已开启（系统指示灯亮着）"和"老师现在听不到你"。
   */
  const micMuted = ref(false)

  /* ---------------------------------------------------------------------- */
  /* 老师私密语音（§25 / §31）                                               */
  /* ---------------------------------------------------------------------- */

  /**
   * 与老师的私密语音（null = 没有这段沟通）。
   *
   * 状态机（纯函数）在 `lib/private-talk.ts`。它只由服务端事件驱动：
   * `PRIVATE_TALK_REQUEST`（要我开麦吗）、`PRIVATE_TALK_STARTED`（老师选中了我）、
   * `PRIVATE_TALK_ENDED`（结束）。学生端**不能**自己开始或结束这段沟通——
   * §31 的模型是"老师选择学生"，而订阅权限也在服务端。
   */
  const privateTalk = ref<StudentPrivateTalk | null>(null)
  /**
   * 老师私密语音的音频播放状态（§31/§32）。
   *
   * `blocked` 时界面必须说出来并给一个"点击播放"的入口，否则学生会在提示
   * "正在与老师语音沟通"的情况下什么都听不到（见 TeacherAudioState 的说明）。
   */
  const teacherAudio = ref<TeacherAudioState>('idle')

  /* ---------------------------------------------------------------------- */
  /* 只存在于内存的私有内容（绝不进 state，见文件头第 1 点）                  */
  /* ---------------------------------------------------------------------- */

  /** join 下发的凭据。离开页面即随 store 一起消失，不落任何存储。 */
  let credentials: MediaCredentials | null = null
  /** 本会话对应的课堂 id（轮询要用；join 响应里没有它）。 */
  let classroomId: string | null = null
  /**
   * 当前持有的屏幕捕获（Phase 5 Gate 的产物）。
   *
   * 刻意**不是** ref：MediaStreamTrack 是浏览器宿主对象，一旦被 Vue 代理就会出现
   * `Illegal invocation` 这类问题（详见 `screen-share.ts` 里 markRaw 的说明），
   * 而且界面不需要读这个对象本身（§56：不显示自身预览，只显示状态）。
   */
  let capture: ScreenCapture | null = null
  let room: ScreenPublisherRoom | null = null
  /**
   * 当前持有的摄像头采集（§24）。
   *
   * 与 `capture` 一样刻意**不是** ref：MediaStreamTrack 是浏览器宿主对象，
   * 被 Vue 代理后会出现 `Illegal invocation`（详见 screen-share.ts 里那段说明）。
   * 界面需要的只是"开没开"（`cameraState`）与那条流（`cameraStream`，已 markRaw）。
   */
  let cameraCapture: CameraCapture | null = null
  let unsubscribeCameraEnded: (() => void) | null = null
  /**
   * 当前持有的麦克风采集（§25）。
   *
   * 与 `capture` / `cameraCapture` 一样刻意**不是** ref：MediaStreamTrack 是浏览器
   * 宿主对象，被 Vue 代理后会出现 `Illegal invocation`（详见 screen-share.ts 里那段说明）。
   * 界面需要的只是"开没开"（`micState`）与"是不是静音"（`micMuted`）——
   * §56 的线框图里麦克风就只有一个开关，没有音量、没有波形。
   */
  let micCapture: MicrophoneCapture | null = null
  let unsubscribeMicEnded: (() => void) | null = null
  /**
   * 麦克风请求序号（与 `cameraSeq` 同一套理由）。
   *
   * `getUserMedia` 与随后的 publish 都是异步的，而学生可以在几百毫秒里点开又点关；
   * 没有序号的话，"关闭"之后才回来的那条轨道会覆盖掉关闭动作的结果——界面显示已关闭，
   * 而系统麦克风指示灯还亮着（那是学生最敏感的一种状态）。晚到的结果一律作废
   * 并**立刻释放**它拿到的那条轨道。
   */
  let micSeq = 0
  /**
   * 摄像头请求序号。
   *
   * WHY 需要它：`getUserMedia` 与随后的 publish 都是异步的，而学生可以在几百毫秒里
   * 点开又点关。没有序号的话，"关闭"之后才回来的那条轨道会覆盖掉关闭动作的结果——
   * 界面显示已关闭，设备却亮着灯。每次状态迁移都让序号自增，晚到的结果一律作废
   * 并**立刻释放**它拿到的那条轨道。
   */
  let cameraSeq = 0
  /** 房间事件与轨道 ended 的取消订阅函数；退出时必须逐个调用。 */
  let roomUnsubscribers: (() => void)[] = []
  let unsubscribeEnded: (() => void) | null = null
  /** 课堂状态**兜底**轮询定时器（实时通道断了的时候它是唯一的发现途径，§47/§49）。 */
  let pollTimer: ReturnType<typeof setInterval> | null = null
  /** 当前定时器用的间隔，用来判断"通道状态变了要不要重建定时器"。 */
  let pollIntervalMs = 0
  /** 轮询序号：晚发出的响应才能写状态，避免乱序覆盖。 */
  let pollSeq = 0
  /** leave 只上报一次（离开按钮 + 组件卸载可能都会触发）。 */
  let leaveReported = false

  const isOnline = computed(() => phase.value === 'online')
  const isScreenLost = computed(() => phase.value === 'screen-lost')
  const isConnecting = computed(
    () => phase.value === 'connecting' || phase.value === 'prepared' || busy.value,
  )
  const isClosedByTeacher = computed(() => phase.value === 'closed')
  const hasNoSession = computed(() => phase.value === 'no-session')
  const hasFailure = computed(() => failure.value !== null)
  /** 界面是否需要显示"重试进入课堂"（媒体失败；屏幕丢失走另一条恢复路径）。 */
  const canRetry = computed(() => phase.value === 'media-error')
  /**
   * 本地已经重新共享、但服务端还没确认（§46）。
   *
   * 这是"不能撒谎"最典型的一处：publish 在本地成功只说明**我们**发出去了，
   * 老师那边有没有收到要以服务端的 `SCREEN_RESTORED` 为准。
   */
  const awaitingScreenRestore = computed(
    () => screenLostByServer.value && (phase.value === 'online' || phase.value === 'connecting'),
  )

  /**
   * 摄像头入口是否可见（§24：**进入课堂之后**才显示）。
   *
   * WHY 用 phase 而不是"页面上有按钮"：§24 的原话是"Student 已经成功进入课堂以后
   * 才显示 📷 开启摄像头"，也就是**不允许**在 PreJoin / 课堂详情页出现这个入口。
   * `online` 与 `screen-lost` 都算"已经在课堂里"：摄像头是可选设备（§21），
   * 屏幕断了不代表摄像头开不了，把入口一起收掉反而是把两件事混为一谈。
   */
  const canUseCamera = computed(() => phase.value === 'online' || phase.value === 'screen-lost')
  const isCameraOn = computed(() => cameraState.value === 'on')
  const isCameraRequesting = computed(() => cameraState.value === 'requesting')
  const hasCameraFailure = computed(() => cameraFailure.value !== null)
  /** 按钮文案（§56 的线框图是「摄像头 [开启]」）。 */
  const cameraActionLabel = computed(() => (isCameraOn.value ? '关闭摄像头' : '📷 开启摄像头'))

  /**
   * 麦克风入口是否可见（§25）。
   *
   * 与摄像头同一条判据（进入课堂之后才显示）：§25 的 `🎤 开启麦克风` 出现在会话页，
   * 而发布一条麦克风轨道必须有房间。**不能**在 PreJoin 就显示它——那会在学生还没
   * 进课堂、也还没看到 §57 的隐私告知之前诱导他授权麦克风。
   */
  const canUseMicrophone = computed(() => phase.value === 'online' || phase.value === 'screen-lost')
  const isMicOn = computed(() => micState.value === 'on')
  const isMicRequesting = computed(() => micState.value === 'requesting')
  const hasMicFailure = computed(() => micFailure.value !== null)
  /** 按钮文案（§25 的 `🎤 开启麦克风`）。 */
  const micActionLabel = computed(() => (isMicOn.value ? '关闭麦克风' : '🎤 开启麦克风'))

  /** 是否需要显示"老师请你开麦"的提示（§25 的 `[开启麦克风] [暂不开启]`）。 */
  const hasTalkRequest = computed(() => privateTalk.value?.requestPending === true)
  /**
   * 老师正在与我语音沟通（§25 的持续指示）。
   *
   * 判据是"这段沟通存在"，而不是"我已开麦"：老师可以向未开麦的学生单向讲话，
   * 而 §25 要求这种单向关系被如实显示出来（见 hasPrivateTalk 的说明）。
   */
  const isTalkingWithTeacher = computed(() => hasPrivateTalk(privateTalk.value))
  /** 提示标题：「王老师希望与你进行语音沟通。」 */
  const talkRequestTitle = computed(() =>
    privateTalk.value === null ? '' : privateTalkRequestTitle(privateTalk.value),
  )
  /** 持续指示：「正在与王老师语音沟通」。 */
  const talkActiveText = computed(() =>
    privateTalk.value === null ? '' : privateTalkActiveText(privateTalk.value),
  )
  /** 学生的麦克风是否已经被老师听到（开着且没有自我静音）。 */
  const isMicrophoneAudible = computed(() => isMicOn.value && !micMuted.value)

  /* ---------------------------------------------------------------------- */
  /* 内部工具                                                                */
  /* ---------------------------------------------------------------------- */

  /**
   * 记一条失败。
   *
   * 只记录**类别**，中文文案来自词表：原始异常对象（可能带着 wss 地址、SDK 内部
   * 报文，理论上也可能带上凭据片段）绝不出现在 state、界面或日志里（§44 / §58）。
   * 排障只保留错误名，够定位"是连不上还是被拒绝"，又不泄漏任何值。
   */
  function fail(kind: MediaFailureKind, cause?: unknown): void {
    failure.value = { kind, message: describeMediaFailure(kind) }
    if (cause !== undefined) {
      // 只留错误名：异常对象可能带着 wss 地址甚至凭据片段，绝不整体打印（§44）。
      const name = cause instanceof Error ? cause.name : typeof cause
      console.error(`[media-session] ${kind} failed: ${name}`)
    }
  }

  /**
   * 摘掉房间事件的监听（幂等）。
   *
   * 刻意**不**动 `unsubscribeEnded`：那是屏幕轨道的监听，而轨道可能比这次连接活得更久
   * （重试期间轨道仍在手里，重连成功后 `watchCaptureEnded()` 会重新挂上）。
   * 轨道的监听只在 `releaseCapture()` 里摘。
   */
  function detachListeners(): void {
    for (const off of roomUnsubscribers) off()
    roomUnsubscribers = []
  }

  /** 断开媒体连接并摘监听（幂等）。不停止本地轨道——那是释放捕获的事。 */
  async function teardownRoom(): Promise<void> {
    detachListeners()
    const current = room
    room = null
    /**
     * 摄像头与麦克风跟着连接一起放手（见 dropCamera / dropMicrophone）。
     *
     * WHY 放在这里而不是各个调用点：`teardownRoom` 是"这条媒体连接结束了"的唯一出口
     * （退出、重试、课堂关闭、切课堂都走它）。分开写就一定会漏掉其中一条路径，
     * 而漏掉的后果是学生的摄像头灯/麦克风指示灯在课堂结束后继续亮着
     * （§65 Case 14 的同一类问题）。
     */
    dropCamera()
    dropMicrophone()
    if (!current) return
    try {
      await current.disconnect()
    } catch {
      // 已经断开的连接再断一次会抛；退出路径不能被它打断。
    }
  }

  /** 释放屏幕捕获（幂等）。退出、课堂关闭、会话失效都走这里。 */
  function releaseCapture(): void {
    unsubscribeEnded?.()
    unsubscribeEnded = null
    releaseScreenCapture(capture)
    capture = null
  }

  /** 停掉课堂状态兜底轮询（幂等）。 */
  function stopClassroomWatch(): void {
    if (pollTimer === null) return
    clearInterval(pollTimer)
    pollTimer = null
    pollIntervalMs = 0
  }

  /**
   * 等待一次"尽力而为"的上报，但设上限。
   *
   * WHY 需要上限：学生点「离开课堂」时，界面必须马上断开媒体、停止共享并回列表。
   * 如果 leave 请求卡在一个丢包的网络里，无限等待就等于"学生已经决定离开，
   * 屏幕却还在被共享"。超时后照常完成本地退出；服务端另有 LiveKit 的
   * participant_left / track_unpublished 兜底（§45/§46）。
   */
  async function settleWithin(request: Promise<void>, timeoutMs: number): Promise<void> {
    let timer: ReturnType<typeof setTimeout> | null = null
    try {
      await Promise.race([
        request.catch(() => undefined),
        new Promise<void>((resolve) => {
          timer = setTimeout(resolve, timeoutMs)
        }),
      ])
    } finally {
      if (timer !== null) clearTimeout(timer)
    }
  }

  /** leave 上报（幂等、失败不抛）。 */
  async function reportLeave(): Promise<void> {
    const id = sessionId.value
    if (id === null || leaveReported) return
    leaveReported = true
    await leaveSession(id)
  }

  /* ---------------------------------------------------------------------- */
  /* 进入课堂                                                                */
  /* ---------------------------------------------------------------------- */

  /**
   * 接收 PreJoin 交接过来的会话（§18 的最后一步）。
   *
   * 同步函数：PreJoin 在拿到 join 响应之后立刻调用它，然后 `router.push`。
   * 之所以不在这里连接媒体，是因为"跳转前就开始连接"会让失败发生在
   * 一个即将卸载的页面里——错误提示会随着导航一起消失。
   *
   * 若 store 里还残留着上一次会话（学生在两个课堂之间来回），先把本地资源
   * 全部清掉：§50 规定同一个 student + run 只允许一个 Active Media Session，
   * 服务端在新连接到来时会取代旧连接，前端要做的只是**不留残骸**。
   */
  function prepare(input: PrepareInput): void {
    stopClassroomWatch()
    void teardownRoom()
    releaseCapture()

    sessionId.value = input.sessionId
    classroomId = input.classroomId
    credentials = input.credentials
    capture = input.capture
    classroom.value = null
    failure.value = null
    quality.value = 'unknown'
    screenLostByServer.value = false
    leaveReported = false
    /**
     * 可选设备与私密语音都从零开始：`teardownRoom()` 已经同步释放了设备（见那边），
     * 这里显式把**状态**也归零，否则上一个课堂的"麦克风已开启""正在与老师语音沟通"
     * 会跟着学生进入下一个课堂。
     */
    micState.value = 'off'
    micFailure.value = null
    clearPrivateTalk()
    phase.value = 'prepared'
  }

  /** §22：屏幕轨道结束。 */
  function handleScreenEnded(): void {
    unsubscribeEnded?.()
    unsubscribeEnded = null
    /**
     * WHY 不 republish：`ended` 表示这条 MediaStreamTrack 已经终止，它不会再有
     * 任何一帧。重新 publish 同一条轨道只会让老师那端出现一个"在线"但永远黑屏的
     * 学生，比明确进入 SCREEN_LOST 糟糕得多（§22 要求的是学生重新共享整屏）。
     * 因此这里只做三件事：撤下已死的发布（尽力）、清空本地引用、进入提示态。
     */
    const dead = capture
    capture = null
    if (dead !== null) releaseScreenCapture(dead)
    if (room !== null) {
      void room.unpublishScreenTrack().catch(() => undefined)
    }
    if (phase.value === 'online' || phase.value === 'connecting') {
      phase.value = 'screen-lost'
    }
  }

  /** 给当前捕获挂上 §22 的 ended 监听（每次 publish 都要重新挂）。 */
  function watchCaptureEnded(): void {
    const current = capture
    if (current === null) return
    unsubscribeEnded?.()
    unsubscribeEnded = current.onEnded(() => {
      // 身份判断：回调可能来自一条已经被替换掉的旧轨道（同一页面里重新共享过）。
      if (capture !== current) return
      handleScreenEnded()
    })
  }

  /* ---------------------------------------------------------------------- */
  /* 摄像头（§24 / §56）                                                     */
  /* ---------------------------------------------------------------------- */

  /**
   * 释放摄像头采集（幂等）。
   *
   * WHY 一定要真的 `stop()` 而不是只 unpublish：`stop()` 是唯一能让**操作系统级**
   * 摄像头指示灯灭掉的动作。只撤下发布的话，学生会看到一个"已关闭"的界面，
   * 而摄像头灯还亮着——那是最直接的一种信任崩塌（他会认为自己在被偷看）。
   * 采集层保证 `stop()` 幂等，所以这里可以无脑调用。
   */
  function releaseCamera(): void {
    unsubscribeCameraEnded?.()
    unsubscribeCameraEnded = null
    releaseCameraCapture(cameraCapture)
    cameraCapture = null
    cameraStream.value = null
  }

  /**
   * 连接已经不可用时丢弃摄像头（**不**试图 unpublish：连接没了，没有信令通道）。
   *
   * WHY 连接一断就必须放手：`on` 的含义是"已发布、老师看得见"。连接断了还留着
   * 一条 live 轨道，界面会继续显示"摄像头 已开启"，而老师那端什么都没有。
   */
  function dropCamera(): void {
    cameraSeq += 1
    releaseCamera()
    cameraState.value = 'off'
    cameraFailure.value = null
  }

  /**
   * 开启摄像头（§24：**只有学生点击**才会走到这里）。
   *
   * 顺序不能变：先申请设备 → 再发布。反过来的话，publish 成功而设备授权失败，
   * 老师那端会先看到一个空的摄像头位。
   *
   * 这个函数**从不抛出**，也**从不触碰** phase / screen 相关的状态：摄像头失败
   * 必须完全局限在 `cameraState` / `cameraFailure` 里（§21：屏幕才是 mandatory）。
   */
  async function enableCamera(): Promise<void> {
    // 重入保护：requesting 期间的重复点击必须是空操作（否则每次点击都是一次设备请求）。
    if (cameraState.value === 'requesting' || cameraState.value === 'on') return
    const active = room
    /**
     * 没有媒体连接时**不**申请设备权限。
     *
     * WHY 这条守卫放在最前面：一条没有房间可发布的轨道只会亮着摄像头灯，却谁也看不到。
     * 界面上这个入口本来就只在课堂里出现（见 `canUseCamera`），这里是那道不变量。
     */
    if (active === null || !canUseCamera.value) return

    const seq = ++cameraSeq
    cameraState.value = 'requesting'
    cameraFailure.value = null

    /** 这次开启是否已经被放弃（学生点了关闭、离开课堂、或媒体连接断了）。 */
    const abandoned = (): boolean => seq !== cameraSeq || room !== active

    try {
      const next = await requestCamera()

      /**
       * 请求飞行期间被放弃：这条轨道**没有主人**了，必须立刻释放。
       * 少了这一句就会出现"学生已经关了摄像头，灯却亮着"。
       */
      if (abandoned()) {
        releaseCameraCapture(next)
        return
      }

      cameraCapture = next
      cameraStream.value = markRaw(next.stream)
      unsubscribeCameraEnded = next.onEnded(() => {
        // 身份判断：回调可能来自一条已经被替换掉的旧轨道。
        if (cameraCapture !== next) return
        /**
         * 设备被拔出 / 被系统或别的程序抢走。轨道已经死了，界面必须回到"没开"，
         * 并给出一句解释——继续显示"已开启"会让老师盯着一个空位。
         */
        releaseCamera()
        /**
         * 还要把这条已经死掉的**发布**撤下来（与 §22 的屏幕丢失同一种处理）。
         *
         * WHY 不能只 stop 本地轨道：本地放手之后，SFU 里仍留着一条没有轨道在推流的
         * camera 发布，服务端的 `track_unpublished` webhook 便永远不会到——
         * 于是老师那端的 `camera.active` 一直停在 true，画中画钉在最后一帧上。
         */
        void active.unpublishCameraTrack().catch(() => undefined)
        cameraState.value = 'error'
        cameraFailure.value = toCameraFailure('track-ended')
      })

      await active.publishCameraTrack(next.track)

      /**
       * 发布完成才发现这次开启已经被放弃（学生点得太快）：把刚发布的轨道撤下来。
       * 不做这一步，服务端会留着一条没有本地轨道在推流的 camera 发布，
       * 老师那端就会出现一个永远黑屏的画中画。
       */
      if (abandoned()) {
        await active.unpublishCameraTrack().catch(() => undefined)
        return
      }

      cameraState.value = 'on'
    } catch (cause) {
      // 晚到的失败同样作废：它属于一次已经被放弃的开启。
      if (abandoned()) return
      releaseCamera()
      cameraState.value = 'error'
      const kind = toCameraFailureKindOf(cause)
      cameraFailure.value = toCameraFailure(kind)
      // 只留错误名：异常对象可能带上设备信息（§44 的"凭据不进日志"同样适用于设备）。
      const name = cause instanceof Error ? cause.name : typeof cause
      console.error(`[media-session] camera failed: ${name}`)
    }
  }

  /**
   * 关闭摄像头：**unpublish + 真正 stop 本地轨道**（§24 的"允许 开 / 关 / 再开"）。
   *
   * WHY 必须两步都做：
   * - 只 stop 不 unpublish：老师那端留着一条永远黑屏的发布；
   * - 只 unpublish 不 stop：摄像头指示灯不灭（见 releaseCamera 的说明）。
   *
   * 先发信令再释放设备：即便 unpublish 卡在网络里，本地也必须在同一个 tick 里放手——
   * 学生点了"关闭"，灯不能等到一次网络往返之后才灭。
   */
  function disableCamera(): void {
    if (cameraState.value === 'off' && cameraCapture === null) return
    cameraSeq += 1
    const active = room
    if (active !== null) void active.unpublishCameraTrack().catch(() => undefined)
    releaseCamera()
    cameraState.value = 'off'
    cameraFailure.value = null
  }

  /** 学生点击摄像头开关时的唯一入口。 */
  function toggleCamera(): void {
    if (cameraState.value === 'on') disableCamera()
    else void enableCamera()
  }

  /* ---------------------------------------------------------------------- */
  /* 麦克风（§25 / §31）                                                     */
  /* ---------------------------------------------------------------------- */

  /**
   * 释放麦克风采集（幂等）。
   *
   * WHY 一定要真的 `stop()` 而不是只 unpublish：`stop()` 是唯一能让**操作系统级**
   * 麦克风指示灯灭掉的动作。只撤下发布的话，学生会看到一个"已关闭"的界面，
   * 而系统仍在录音——那是最直接的一种信任崩塌（他会认为自己在被偷听）。
   * 采集层保证 `stop()` 幂等，所以这里可以无脑调用。
   */
  function releaseMicrophone(): void {
    unsubscribeMicEnded?.()
    unsubscribeMicEnded = null
    releaseMicrophoneCapture(micCapture)
    micCapture = null
    // 设备已经不在了：静音状态没有意义，留着会让下一次开麦继承一个"看起来静音"的界面。
    micMuted.value = false
  }

  /**
   * 连接已经不可用时丢弃麦克风（**不**试图 unpublish：连接没了，没有信令通道）。
   *
   * WHY 连接一断就必须放手：`on` 的含义是"已发布、老师听得到"。连接断了还留着
   * 一条 live 轨道，界面会继续显示"麦克风已开启"，而老师那端什么都没有。
   */
  function dropMicrophone(): void {
    micSeq += 1
    releaseMicrophone()
    micState.value = 'off'
    micFailure.value = null
  }

  /** 清掉私密语音（老师关课、学生离开、连接断开时都要清）。 */
  function clearPrivateTalk(): void {
    privateTalk.value = endPrivateTalk()
    teacherAudio.value = 'idle'
  }

  /**
   * 把"现在是不是静音"写到本地轨道上（§31）。
   *
   * 没有麦克风时是空操作：静音只对一条已经存在的采集有意义。
   */
  function syncMicrophoneMute(): void {
    applyMicrophoneMute(micCapture, micMuted.value)
  }

  /**
   * 开启麦克风（§25：**只有学生点击**才会走到这里）。
   *
   * 顺序不能变：先申请设备 → 再发布。反过来的话，publish 成功而设备授权失败，
   * 老师那端会先看到一条麦克风发布再消失。
   *
   * 这个函数**从不抛出**，也**从不触碰** phase / screen 相关的状态：麦克风失败
   * 必须完全局限在 `micState` / `micFailure` 里（§21/§25）。
   */
  async function enableMicrophone(): Promise<void> {
    // 重入保护：requesting 期间的重复点击必须是空操作（否则每次点击都是一次设备请求）。
    if (micState.value === 'requesting' || micState.value === 'on') return
    const active = room
    /**
     * 没有媒体连接时**不**申请设备权限。
     *
     * WHY 这条守卫放在最前面：一条没有房间可发布的轨道只会亮着麦克风指示灯，
     * 却谁也听不到。界面上这个入口本来就只在课堂里出现（见 `canUseMicrophone`），
     * 这里是那道不变量。§25 也是这个意思：只有学生点击、且课堂在运行时才请求。
     */
    if (active === null || !canUseMicrophone.value) return

    const seq = ++micSeq
    micState.value = 'requesting'
    micFailure.value = null

    /** 这次开启是否已经被放弃（学生点了关闭、离开课堂、或媒体连接断了）。 */
    const abandoned = (): boolean => seq !== micSeq || room !== active

    try {
      const next = await requestMicrophone()

      /**
       * 请求飞行期间被放弃：这条轨道**没有主人**了，必须立刻释放。
       * 少了这一句就会出现"学生已经关了麦克风，系统却还在录音"。
       */
      if (abandoned()) {
        releaseMicrophoneCapture(next)
        return
      }

      micCapture = next
      // 新设备从"不静音"开始：上一次的静音状态不该跨设备继承。
      micMuted.value = false
      unsubscribeMicEnded = next.onEnded(() => {
        // 身份判断：回调可能来自一条已经被替换掉的旧轨道。
        if (micCapture !== next) return
        /**
         * 设备被拔出 / 被系统或别的程序抢走。轨道已经死了，界面必须回到"没开"，
         * 并给出一句解释——继续显示"已开启"会让老师对着一个没有声音的麦克风讲话。
         */
        releaseMicrophone()
        /**
         * 还要把这条已经死掉的**发布**撤下来（与 §22 的屏幕丢失同一种处理）：
         * 本地放手之后，SFU 里仍留着一条没有轨道在推流的 microphone 发布，
         * 服务端的 `track_unpublished` webhook 便永远不会到——老师那端的
         * `microphone.active` 会一直停在 true。
         */
        void active.unpublishMicrophoneTrack().catch(() => undefined)
        micState.value = 'error'
        micFailure.value = toMicFailure('track-ended')
      })

      await active.publishMicrophoneTrack(next.track)

      /**
       * 发布完成才发现这次开启已经被放弃（学生点得太快）：把刚发布的轨道撤下来。
       * 不做这一步，服务端会留着一条没有本地轨道在推流的 microphone 发布。
       */
      if (abandoned()) {
        await active.unpublishMicrophoneTrack().catch(() => undefined)
        return
      }

      micState.value = 'on'
      /**
       * 麦克风真的开了：§25 要求"开启成功后提示会变成老师正在与你语音沟通"，
       * 而"还在等你决定要不要开麦"这一半必须立刻收掉（见 markMicrophoneEnabled）。
       */
      privateTalk.value = markMicrophoneEnabled(privateTalk.value)
    } catch (cause) {
      // 晚到的失败同样作废：它属于一次已经被放弃的开启。
      if (abandoned()) return
      releaseMicrophone()
      micState.value = 'error'
      const kind = toMicFailureKindOf(cause)
      micFailure.value = toMicFailure(kind)
      // 只留错误名：异常对象可能带上设备信息（§44 的"凭据不进日志"同样适用于设备）。
      const name = cause instanceof Error ? cause.name : typeof cause
      console.error(`[media-session] microphone failed: ${name}`)
    }
  }

  /**
   * 关闭麦克风：**unpublish + 真正 stop 本地轨道**（§25 的"允许 开 / 关 / 再开"）。
   *
   * WHY 必须两步都做：
   * - 只 stop 不 unpublish：老师那端留着一条永远没有声音的发布；
   * - 只 unpublish 不 stop：系统麦克风指示灯不灭（见 releaseMicrophone 的说明）。
   *
   * 先发信令再释放设备：即便 unpublish 卡在网络里，本地也必须在同一个 tick 里放手——
   * 学生点了"关闭"，录音不能等到一次网络往返之后才停。
   */
  function disableMicrophone(): void {
    if (micState.value === 'off' && micCapture === null) return
    micSeq += 1
    const active = room
    if (active !== null) void active.unpublishMicrophoneTrack().catch(() => undefined)
    releaseMicrophone()
    micState.value = 'off'
    micFailure.value = null
  }

  /** 学生点击麦克风开关时的唯一入口。 */
  function toggleMicrophone(): void {
    if (micState.value === 'on') disableMicrophone()
    else void enableMicrophone()
  }

  /**
   * 「开启麦克风」（老师请求提示里的那个按钮，§25）。
   *
   * WHY 与普通的开关分开一个入口：这里的语义是"回应老师的请求"，
   * 而且**必须由用户点击触发**——§25 明确老师不能绕过浏览器权限远程打开麦克风。
   * 所以这个函数只做一件事：走一遍 `enableMicrophone()`；它绝不被事件自动调用。
   * 麦克风已经开着时按钮不该出现，真的出现也只把提示收掉（幂等）。
   */
  function acceptTalkRequest(): void {
    if (micState.value === 'on') {
      privateTalk.value = markMicrophoneEnabled(privateTalk.value)
      return
    }
    void enableMicrophone()
  }

  /**
   * 「暂不开启」（§25）。
   *
   * 只关掉提示，**不结束这段沟通**：老师仍然可以单向对学生讲话，
   * 界面必须如实说明这一点（见 PRIVATE_TALK_DECLINE_NOTE）。
   */
  function declineTalk(): void {
    privateTalk.value = declineTalkRequest(privateTalk.value)
  }

  /**
   * 本地静音/取消静音自己（§31 的"静音自己"）。
   *
   * 只在真的开着麦克风时有意义；没有设备时按钮本来也不显示，这里是第二道守卫。
   */
  function toggleSelfMute(): void {
    if (!isMicOn.value) return
    micMuted.value = !micMuted.value
    syncMicrophoneMute()
  }

  /**
   * 在用户手势里恢复老师语音的播放（§31/§32）。
   *
   * 浏览器自动播放策略会拦住没有手势的音频；`blocked` 状态下必须由学生点一下。
   * 没有房间时是空操作。
   */
  async function resumeTeacherAudio(): Promise<void> {
    const active = room
    if (active === null) return
    await active.resumeTeacherAudio()
  }

  /** 订阅房间事件：断线、重连、连接质量。 */
  function subscribeRoomEvents(active: ScreenPublisherRoom): void {
    roomUnsubscribers.push(
      active.onQualityChanged((next) => {
        quality.value = next
      }),
      active.onDisconnected(() => {
        /**
         * 媒体连接断了（网络、token 过期、房间被服务端终止……）。
         *
         * 这里**不**把 phase 改成 left/closed：那是别人的判断（学生点了离开、
         * 或轮询发现课堂 CLOSED）。媒体层只报告"我断了"，界面因此显示
         * media-error 并可重试。若原因是老师关课堂，轮询随后会把 phase 改成
         * `closed` 并给出 §49 的专用提示。
         */
        if (phase.value === 'left' || phase.value === 'closed') return
        reconnecting.value = false
        fail('disconnected')
        phase.value = 'media-error'
        /**
         * 摄像头必须一起放手：`on` 的含义是"已发布、老师看得见"，而这条连接已经没了。
         * 界面那句"媒体连接有问题"已经解释了发生什么，这里不再叠一条摄像头文案。
         * （屏幕轨道刻意**不**在这里释放：它是学生重新共享时要复用的那条，见 §22。）
         */
        dropCamera()
        /**
         * 麦克风与私密语音一起放手：连接没了，`on` 就不再成立（"已开启、老师听得到"），
         * 而且那段语音沟通的音频也已经不可能再出声。
         */
        dropMicrophone()
        clearPrivateTalk()
      }),
      /**
       * 网络抖动时 LiveKit 会自己重连（§52：不要在 V1 自己实现重连策略）。
       * 前端只把这件事显示出来，不介入重连过程——学生看到的应该是
       * "正在重连"，而不是一个突然变红、几秒后又变绿的课堂。
       */
      active.onReconnecting(() => {
        reconnecting.value = true
      }),
      active.onReconnected(() => {
        reconnecting.value = false
        // 重连成功即回到正常：此前那条 disconnected 的失败提示已经不成立了。
        if (phase.value === 'media-error' && failure.value?.kind === 'disconnected') {
          failure.value = null
          phase.value = 'online'
        }
      }),
      /**
       * 老师私密语音的播放状态（§31/§32）。
       *
       * 这条订阅是**服务端**决定的（UpdateSubscriptions 只让目标学生收到老师的麦克风），
       * 所以学生端没有任何"订阅老师"的动作；这里只如实反映"能不能听到"。
       * `blocked` 时界面会给一个"点击播放声音"的入口（自动播放策略的要求）。
       */
      active.onTeacherAudioChanged((next) => {
        teacherAudio.value = next
      }),
    )
  }

  /**
   * 连接媒体并发布屏幕轨道。
   *
   * 失败时**保留本地捕获**：Gate 已经通过，学生不必再经历一次授权弹窗；
   * 重试只需重新 join（拿新 token）再 publish。
   */
  async function connectAndPublish(): Promise<void> {
    const active = credentials
    const current = capture
    if (active === null) {
      phase.value = 'no-session'
      return
    }
    if (current === null) {
      // 没有轨道可发布（例如刷新后凭据还在、但捕获早没了）。
      phase.value = 'screen-lost'
      return
    }
    /**
     * 轨道在 Gate 与 publish 之间就已经死了（学生在这几秒里点了浏览器的"停止共享"）。
     * 这种情况必须走 SCREEN_LOST：publish 一条已结束的轨道只会让老师那端多一个
     * 永远黑屏的"在线"学生（§22）。
     */
    if (current.track.readyState === 'ended') {
      releaseCapture()
      phase.value = 'screen-lost'
      return
    }

    phase.value = 'connecting'
    failure.value = null
    busy.value = true
    /** 记住失败发生在哪一步：连不上与 publish 被拒要给不同的下一步动作。 */
    let stage: MediaFailureKind = 'connect'
    try {
      const next = await createScreenPublisherRoom(active)
      room = next
      subscribeRoomEvents(next)
      await next.connect()
      stage = 'publish'
      await next.publishScreenTrack(current.track)
      /**
       * 挂 ended 监听。若轨道恰好**在 publish 期间**结束，`onEnded` 会同步回调
       * （已结束的轨道不会再触发事件），`handleScreenEnded` 会把 `capture` 清空——
       * 因此下面用**身份判断**决定要不要宣布 online，而不是无条件设置：
       * 一条已经死掉的轨道进不了"已进入课堂"（§21 的不变量）。
       */
      watchCaptureEnded()
      quality.value = next.connectionQuality()
      if (capture === current) {
        phase.value = 'online'
        startClassroomWatch()
      }
    } catch (cause) {
      await teardownRoom()
      fail(stage, cause)
      phase.value = 'media-error'
    } finally {
      busy.value = false
    }
  }

  /**
   * 会话页挂载时调用：把 prepared 变成 connecting → online。
   *
   * `idle` 表示"内存里根本没有会话"——刷新页面、直接输入 URL、从别的标签页
   * 复制链接都会走到这里。凭据只在内存（§44），所以这**不是**故障，而是一种
   * 必然结果：把它明确标记成 `no-session`，页面才能给出"请重新进入课堂"，
   * 而不是永远停在"连接中…"。
   */
  async function begin(): Promise<void> {
    if (phase.value === 'idle') {
      phase.value = 'no-session'
      return
    }
    if (phase.value !== 'prepared') return
    await connectAndPublish()
  }

  /**
   * 重新共享之后发布**新**轨道（§22 的恢复路径）。
   *
   * 关键点：**不重新 join**。会话还在，参与者身份、权限、房间都没有变，
   * 变的只是"我这边多了一条新的屏幕轨道"。重新 join 会拿到新 token、创建新
   * session（§50 的"新连接取代旧连接"），老师那端会看到学生卡片闪一下，
   * 而这一切对恢复一条轨道毫无帮助。只有在**媒体连接本身**出问题时才需要重连
   * （那条路径在 `retry()` 里）。
   */
  async function publishCapture(next: ScreenCapture): Promise<void> {
    const active = room
    if (active === null || phase.value === 'no-session') {
      // 房间里已经没有连接了：保留这条新轨道，交给 retry() 去重新 join。
      capture = next
      fail('rejoin')
      phase.value = 'media-error'
      return
    }
    releaseCapture()
    capture = next
    failure.value = null
    busy.value = true
    try {
      await active.publishScreenTrack(next.track)
      // 同 connectAndPublish：轨道可能在 publish 期间就结束了，此时不能宣布 online。
      watchCaptureEnded()
      if (capture === next) phase.value = 'online'
    } catch (cause) {
      fail('publish', cause)
      phase.value = 'media-error'
    } finally {
      busy.value = false
    }
  }

  /**
   * 媒体失败后的重试：**重新 join 拿新 token**，再连接、发布。
   *
   * WHY 必须重新 join 而不是"用旧凭据再连一次"：token 是短时凭证（§44），
   * 失败的原因很可能正是它过期或房间已经换了；复用旧 token 重试只会再失败一次，
   * 而学生会以为"重试按钮坏了"。
   */
  async function retry(): Promise<void> {
    if (classroomId === null) {
      phase.value = 'no-session'
      return
    }
    if (capture === null) {
      // 没有活着的轨道：重试也没有东西可发布，让学生先重新共享。
      phase.value = 'screen-lost'
      return
    }

    phase.value = 'connecting'
    failure.value = null
    busy.value = true
    await teardownRoom()
    try {
      const response = await joinClassroom(
        classroomId,
        toJoinCaptureDiagnostics(describeScreenCapture(capture)),
      )
      credentials = { livekitUrl: response.livekitUrl, token: response.token }
      // §50：后端可能返回一个新的 sessionId；视图会把地址栏同步过去。
      sessionId.value = response.sessionId
      leaveReported = false
      await connectAndPublish()
    } catch (cause) {
      fail('rejoin', cause)
      phase.value = 'media-error'
    } finally {
      busy.value = false
    }
  }

  /* ---------------------------------------------------------------------- */
  /* 课堂状态：实时事件为主（§47），低频兜底轮询为辅（§49）                    */
  /* ---------------------------------------------------------------------- */

  /**
   * 实时通道状态变化（由 realtime store 转达）。
   *
   * 唯一用途是**兜底轮询的频率**：通道开着时 60 秒足够，断了就必须收紧，
   * 否则"老师关了课堂"可能被拖到一个很晚的时刻才被发现（见 classroomFallbackPollMs）。
   */
  function setRealtimeState(next: RealtimeConnectionState): void {
    if (realtimeState.value === next) return
    realtimeState.value = next
    syncPollTimer()
  }

  /**
   * 实时事件 → 会话状态（§46 / §49 / §25 / §31）。
   *
   * 只处理"发给学生本人"的事件；其余类型根本不进这里（见 realtime store 的路由）。
   * `PRIVATE_TALK_*` 是 Phase 10 加进来的三兄弟，它们的共同点是**只描述老师与
   * 我之间的那段沟通**：载荷里没有任何其他学生的字段（§26）。
   */
  function applyRealtimeEvent(event: RealtimeEvent): void {
    switch (event.type) {
      case 'ROOM_CLOSED':
        /**
         * 只认自己这间课堂。服务端按 §26 只会推授权课堂，出现别的 id 说明本地
         * 已经不在那间课堂里了（例如刚离开），此时什么都不要做。
         */
        if (classroomId === null || event.data.classroomId !== classroomId) return
        if (phase.value === 'left' || phase.value === 'idle' || phase.value === 'no-session') return
        applyClassroomClosed()
        break
      case 'SCREEN_LOST':
        if (!isCurrentSession(event.data.sessionId)) return
        handleServerScreenLost()
        break
      case 'SCREEN_RESTORED':
        if (!isCurrentSession(event.data.sessionId)) return
        handleServerScreenRestored()
        break
      case 'PRIVATE_TALK_REQUEST':
        /**
         * 老师想和我通话，而我还没开麦（§25：服务端只在学生麦克风未开启时才发它）。
         *
         * WHY 只在这里**显示提示**：§25 明确老师不能绕过浏览器权限远程打开麦克风，
         * 所以收到这条事件绝不等于"开麦"——真正的 `getUserMedia` 只能由学生点击触发
         * （见 acceptTalkRequest）。这里连麦克风状态都不碰。
         */
        if (isSessionFinished()) return
        privateTalk.value = applyTalkRequest(
          privateTalk.value,
          event.data.teacherDisplayName,
          micState.value === 'on',
        )
        break
      case 'PRIVATE_TALK_STARTED':
        /**
         * 老师选中了我（§31）。老师那版的载荷没有 `teacherDisplayName`，学生端只认
         * 学生版——拿到老师版说明这条消息不该发给我，忽略比猜一个姓名诚实。
         */
        if (isSessionFinished()) return
        if (!('teacherDisplayName' in event.data)) return
        privateTalk.value = applyTalkStarted(
          privateTalk.value,
          event.data.teacherDisplayName,
          micState.value === 'on',
        )
        break
      case 'PRIVATE_TALK_ENDED':
        /**
         * 结束（§31 的 TALKING → IDLE）。按 `sessionId` 匹配（就是我自己的会话），
         * 与 SCREEN_LOST 用同一把键：老师版载荷里的 `studentId` 对学生端没有意义。
         */
        if (!isCurrentSession(event.data.sessionId)) return
        clearPrivateTalk()
        break
      default:
        break
    }
  }

  /**
   * 事件是不是在说我**当前**这个会话。
   *
   * WHY 必须比 sessionId：`retry()` 会重新 join，后端可能给出新的 session（§50
   * "新连接取代旧连接"）。那时旧会话的 SCREEN_LOST 会晚到，拿它去改新会话的状态
   * 就是一次纯粹的误报。
   */
  function isCurrentSession(eventSessionId: string): boolean {
    return sessionId.value !== null && sessionId.value === eventSessionId
  }

  /**
   * 服务端判定屏幕丢失（§46 的权威通道）。
   *
   * - 本地可能还在共享（webhook 与浏览器之间本来就可能不同步）：**界面以服务端为准**，
   *   立刻停止显示"正在共享整个屏幕"，并提示重新共享；
   * - 但**不主动杀掉**本地那条轨道：它可能还在发布（服务端漏报），而且停止共享是
   *   一个不可逆的用户可见动作，不应该由一条可能迟到的事件替学生决定。
   *   学生点"重新共享"时，旧轨道会在发布新轨道的那一步被正常释放。
   */
  function handleServerScreenLost(): void {
    screenLostByServer.value = true
    if (phase.value === 'online' || phase.value === 'connecting') {
      phase.value = 'screen-lost'
    }
  }

  /**
   * 服务端确认屏幕恢复（§46）。
   *
   * 只有在**本地确实握着一条活轨道**时才回到"正常显示"：服务端说恢复了、
   * 而我这边没有轨道，那说明恢复的是别的东西（或本地刚释放过），
   * 此时显示"正在共享"就是撒谎（§21 的不变量）。
   */
  function handleServerScreenRestored(): void {
    screenLostByServer.value = false
    if (capture === null) return
    if (phase.value === 'screen-lost') phase.value = 'online'
  }

  /**
   * 老师关闭课堂时的收尾（§49 的学生侧那一半）。
   *
   * 老师关闭课堂 ⇒ 后端已经把 StudentSessions 标记成 ROOM_CLOSED（§49），
   * 所以这里**不需要**再调 leave：那是"学生主动离开"的接口。前端要做的是立刻
   * 停止媒体与共享，并给出「老师已经关闭本课堂」的提示。
   */
  function applyClassroomClosed(): void {
    stopClassroomWatch()
    phase.value = 'closed'
    failure.value = null
    screenLostByServer.value = false
    // 先清掉那段语音沟通：`teardownRoom()` 释放设备与连接，但"正在与老师语音沟通"
    // 是**界面状态**，课堂都结束了还挂着它就是撒谎。
    clearPrivateTalk()
    void teardownRoom()
    releaseCapture()
    credentials = null
  }

  /**
   * 拉一次课堂详情（兜底轮询与"实时通道刚恢复"都会调它）。
   *
   * 失败**不改变**课堂状态：一次网络抖动不等于"课堂关了"，据此断开学生的共享
   * 才是真的错误。等下一次轮询即可。
   */
  async function refreshClassroom(): Promise<void> {
    const id = classroomId
    if (id === null) return
    if (isSessionFinished()) return
    const seq = ++pollSeq
    try {
      const next = await getClassroom(id)
      if (seq !== pollSeq) return
      /**
       * 请求飞行期间实时事件已经把课堂关掉了（§47 的事件比这次快照新）：
       * 这份快照反映的是关课之前的时刻，写进去只会让"课堂已结束"与详情数据打架。
       */
      if (isSessionFinished()) return
      classroom.value = next
      if (next.status === 'CLOSED') applyClassroomClosed()
    } catch {
      // 见上：轮询失败保持现状。
    }
  }

  /** 会话是否已经结束（老师关课或学生离开）；结束后一切快照都不再改状态。 */
  function isSessionFinished(): boolean {
    return phase.value === 'closed' || phase.value === 'left'
  }

  /**
   * 开始兜底轮询。
   *
   * 先立刻拉一次：课堂名要马上显示，而且"老师刚关了课堂"不必等满一个周期。
   * 之后由 `syncPollTimer()` 按实时通道状态决定间隔。
   */
  function startClassroomWatch(): void {
    if (classroomId === null) return
    void refreshClassroom()
    pollIntervalMs = 0
    syncPollTimer()
  }

  /**
   * 按当前实时通道状态重建兜底定时器（幂等：间隔没变就什么都不做）。
   *
   * WHY 两档：通道正常时轮询只是兜底（60 秒），通道断开时它是**唯一**能发现
   * "课堂已关闭"的手段，必须收紧（30 秒）。
   */
  function syncPollTimer(): void {
    if (classroomId === null) return
    const interval = classroomFallbackPollMs(realtimeState.value)
    if (pollTimer !== null && pollIntervalMs === interval) return
    if (pollTimer !== null) clearInterval(pollTimer)
    pollIntervalMs = interval
    pollTimer = setInterval(() => {
      void refreshClassroom()
    }, interval)
  }

  /* ---------------------------------------------------------------------- */
  /* 退出                                                                    */
  /* ---------------------------------------------------------------------- */

  /** 学生主动离开（离开按钮 / 路由离开）。顺序：上报 → 断开媒体 → 停止捕获。 */
  async function leave(): Promise<void> {
    if (phase.value === 'left') return
    const closedByTeacher = phase.value === 'closed'
    stopClassroomWatch()
    phase.value = 'left'
    // 课堂已经被老师关闭时不再上报：后端在 §49 的流程里已经把会话标记成
    // ROOM_CLOSED，再调一次 leave 只会得到一个无意义的 404/409。
    if (!closedByTeacher) await settleWithin(reportLeave(), 1500)
    await teardownRoom()
    releaseCapture()
    clearPrivateTalk()
    credentials = null
    classroomId = null
    failure.value = null
    reconnecting.value = false
    screenLostByServer.value = false
  }

  /**
   * 页面正在卸载时的**同步**兜底（beforeunload / pagehide）。
   *
   * WHY 只能是同步的：`beforeunload` 里发起异步请求，浏览器通常会在请求完成前
   * 就把页面销毁（fetch 会被取消），因此那里既不能 `await` 上报、也不能指望
   * `await room.disconnect()` 跑完。这一段只做两件必须立刻发生的事：
   * 停止本地捕获（否则系统级"正在共享"提示会一直挂着）与发起断开。
   * 服务端最终通过 LiveKit 的 participant_left / track_unpublished 发现学生离线
   * （§45/§46：不要只相信前端主动报告）——这正是那套兜底存在的意义。
   */
  function releaseNow(): void {
    stopClassroomWatch()
    detachListeners()
    const current = room
    room = null
    if (current !== null) {
      try {
        void current.disconnect().catch(() => undefined)
      } catch {
        // 卸载路径上不允许抛错。
      }
    }
    releaseCapture()
    /**
     * 摄像头与麦克风必须在这里同步释放。`stop()` 是同步的，所以卸载路径能真正做到
     * "灯立刻灭"；而断开连接是异步的、很可能跑不完（浏览器会在请求完成前销毁页面）。
     * 麦克风比摄像头更需要这一步：设备不能被后台占着（§25/§31）。
     */
    dropCamera()
    dropMicrophone()
    clearPrivateTalk()
    credentials = null
    classroomId = null
    if (phase.value !== 'closed') phase.value = 'left'
  }

  /**
   * 彻底重置（切换课堂、离开页面后的清理）。
   *
   * 与 `leave()` 的区别：这里不再上报（已经报过了），只保证本地没有残骸——
   * 定时器、监听、轨道、凭据都要清干净，否则下一个课堂会继承上一个课堂的计时器。
   */
  function reset(): void {
    stopClassroomWatch()
    void teardownRoom()
    releaseCapture()
    credentials = null
    classroomId = null
    sessionId.value = null
    classroom.value = null
    quality.value = 'unknown'
    failure.value = null
    busy.value = false
    reconnecting.value = false
    screenLostByServer.value = false
    leaveReported = false
    micState.value = 'off'
    micFailure.value = null
    clearPrivateTalk()
    phase.value = 'idle'
  }

  return {
    phase,
    sessionId,
    classroom,
    quality,
    failure,
    busy,
    reconnecting,
    screenLostByServer,
    realtimeState,
    isOnline,
    isScreenLost,
    isConnecting,
    isClosedByTeacher,
    hasNoSession,
    hasFailure,
    canRetry,
    awaitingScreenRestore,
    cameraState,
    cameraFailure,
    cameraStream,
    canUseCamera,
    isCameraOn,
    isCameraRequesting,
    hasCameraFailure,
    cameraActionLabel,
    toggleCamera,
    disableCamera,
    micState,
    micFailure,
    micMuted,
    canUseMicrophone,
    isMicOn,
    isMicRequesting,
    hasMicFailure,
    micActionLabel,
    isMicrophoneAudible,
    hasTalkRequest,
    isTalkingWithTeacher,
    talkRequestTitle,
    talkActiveText,
    privateTalk,
    teacherAudio,
    toggleMicrophone,
    disableMicrophone,
    acceptTalkRequest,
    declineTalk,
    toggleSelfMute,
    resumeTeacherAudio,
    prepare,
    begin,
    publishCapture,
    retry,
    refreshClassroom,
    startClassroomWatch,
    stopClassroomWatch,
    setRealtimeState,
    applyRealtimeEvent,
    leave,
    releaseNow,
    reset,
  }
})
