import { ApiError, isApiError, type RealtimeConnectionState } from '@classwatch/api-client'
import type {
  MonitorStudent,
  MonitorTileState,
  PrivateTalkTarget,
  RealtimeEvent,
  ScreenStateData,
  StudentOfflineReason,
  StudentSessionStatus,
  TeacherScreenStateData,
} from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, markRaw, ref, shallowRef } from 'vue'
import {
  createMonitorRoom,
  type CameraSubscription,
  type MediaCredentials,
  type MicrophoneSubscription,
  type MonitorRoom,
  type ScreenQuality,
  type ScreenSubscription,
} from '../lib/media/media-room.ts'
import { shouldSubscribeCamera } from '../lib/camera-pip.ts'
import {
  deriveMonitorTileState,
  isStudentEntered,
  monitorFallbackPollMs,
  shouldSubscribeMicrophone,
  shouldSubscribeScreen,
  type MonitorBadge,
  type MonitorMediaState,
  describeMonitorBadge,
} from '../lib/monitor-status.ts'
import {
  describeTeacherMicFailure,
  releaseTeacherMicrophone,
  requestTeacherMicrophone,
  toTeacherMicFailureKindOf,
  type TeacherMicrophoneCapture,
  type TeacherMicState,
} from '../lib/teacher-mic.ts'
import {
  canStartPrivateTalk,
  toPrivateTalkFailure,
  type PrivateTalkFailure,
} from '../lib/private-talk.ts'
import { getPrivateTalk, startPrivateTalk, stopPrivateTalk } from '../lib/private-talk-api.ts'
import { getMonitor, requestMediaToken } from '../lib/teacher-monitor-api.ts'

/**
 * 老师端监督 store（§27 / §29 / §30 / §51 / §52）。
 *
 * 它把两路**完全不同**的信息合成监督墙需要的形状：
 *
 * ```text
 * GET /monitor（首屏快照 + 兜底）        /ws/teacher（事件增量）      LiveKit（媒体事实）
 *        ↓                                   ↓                            ↓
 *   students: MonitorStudent[] ──────────────┘                mediaStates / subscriptions
 *        └──────────────────────────┬───────────────────────────────────┘
 *                              Monitor Card（视图）
 * ```
 *
 * 八条纪律：
 *
 * 1. **业务状态只用 DTO**（§51）。`students` 只会被 monitor 响应整体替换，
 *    绝不因为"房间里还有这个 participant"就把某个学生标成在线。
 * 2. **订阅是一个"计划"，不是一串动作**（§52）。可见性、Focus、页面隐藏三路输入
 *    先合成"现在该订谁、订多清楚"，再由 `applySubscriptionPlan()` 一次性收敛。
 *    三路各自去调订阅，就会出现"滚动时订阅/退订互相打架"这类只在真机上偶发的 bug。
 * 3. **凭据只存内存**（§44）：`credentials` 是闭包变量，不进响应式 state。
 * 4. **断线是可恢复的失败**：媒体连接断开 → `mediaError` + 重试入口（重新申请
 *    media-token）；而不是把整页变成空白。
 * 5. **事件增量更新，快照兜底**（§47 / §51）。Phase 8 起"谁上线了、谁的屏幕断了"
 *    由 `/ws/teacher` 推过来，`GET monitor` 退回它真正不可替代的位置：
 *    首屏快照 + **名单的权威**（新学生被加入课堂不会发事件）+ 兜底。
 *    兜底间隔分两档：通道正常 60 秒（只是防漏），通道断开 20 秒（这段时间里
 *    快照是唯一的信息来源，见 monitorFallbackPollMs）。
 * 6. **一次只跑一轮协调**：三路输入都会触发协调，交错执行会让"该订的集合"在两次
 *    await 之间变化（重复退订、把刚订好的又退掉）。串行化 + 合并重跑是最省心的写法。
 * 7. **事件不得凭空造数据**（§80）。事件只更新**已经在名单里**的学生：
 *    载荷里没有 `connection`、`joinedAt` 这些字段，靠默认值补出来就是假数据；
 *    真正的新增学生一律走快照刷新（名单只有后端说了算）。
 * 8. **Focus 放在 store 而不是视图**：它不是纯视图开关，而是直接决定订阅画质
 *    （§30 的"Focus 优先较高画质"）。放在视图里，画质决策就会分裂成两处。
 * 9. **音频只订一路，而且只订 Focus 那一个**（§32）。`desiredMicrophoneIdentity()`
 *    返回单值而不是集合，让"网格里绝不同时开多路音频"这条不变量在结构上成立——
 *    多路混在一起之后老师分辨不出是谁在说话，这条能力本身就没有意义了。
 *    页面上必须**写出来**在听谁：音频是看不见的，界面得替老师记住这件事。
 * 10. **私密语音是服务端状态**（§31）：`talkTarget` 只会被 POST/GET 响应与
 *    `PRIVATE_TALK_STARTED/ENDED` 改写，切换目标就是再 POST 一次（后端负责撤销旧的）。
 *    本地绝不拼一个目标出来——拼错 sessionId 会让界面显示"正在与张三讲话"，
 *    而声音发给了别人。
 */
export const useMonitorStore = defineStore('teacher-monitor', () => {
  /** 兜底快照的合并窗口：一次事件风暴（10 个人同时进来）只换一次请求。 */
  const SNAPSHOT_REFRESH_DEBOUNCE_MS = 500

  /**
   * 网格卡片的画质档（§52）。
   *
   * WHY 网格必须是低画质：监督墙同时挂十几到几十张卡片，如果每张都按 1080p 下行，
   * 老师一侧的下行带宽与 SFU 的转发成本都会被打爆（§52 开头就是"第一版不要让老师
   * 同时下载 30 × 1080p"）。卡片上那块画面只有几百像素宽，高画质也看不出差别。
   * Focus 是唯一需要看清细节的场景（读代码、看文档），所以只有它升到 high。
   */
  const GRID_QUALITY: ScreenQuality = 'low'
  const FOCUS_QUALITY: ScreenQuality = 'high'

  /* ---------------------------------------------------------------------- */
  /* 界面可读的状态                                                          */
  /* ---------------------------------------------------------------------- */

  /** 业务状态（§51）：每次刷新整体替换，不做增量合并。 */
  const students = ref<MonitorStudent[]>([])
  const loading = ref(false)
  const loaded = ref(false)
  /** monitor 接口失败（业务面）。 */
  const error = ref<ApiError | null>(null)
  /** 媒体面失败的中文提示（连不上 / 订阅不上）；与 error 分开，因为重试的对象不同。 */
  const mediaError = ref<string | null>(null)
  /** 媒体是否已经连上（决定页面上是"等待共享"还是"连接中"）。 */
  const mediaConnected = ref(false)

  /**
   * 每个学生的媒体订阅状态，键是 sessionId（= LiveKit identity）。
   *
   * 只有这一层是响应式的：订阅对象本身持有浏览器媒体对象，必须 markRaw
   * （见 subscriptions 的说明）。
   */
  const mediaStates = ref<Record<string, MonitorMediaState>>({})

  /**
   * 已建立的订阅：sessionId → ScreenSubscription。
   *
   * WHY `shallowRef` + `markRaw`：订阅对象闭包持有 LiveKit 的 RemoteVideoTrack。
   * 那是浏览器宿主对象，被 Vue 深度代理后会出各种 `Illegal invocation`
   * 与身份比较失真（详见学生端 screen-share store 里那段说明）。
   */
  const subscriptions = shallowRef<Record<string, ScreenSubscription>>({})

  /**
   * 每条订阅当前的画质档（键同样是 identity）。
   *
   * 存一份是因为"该订多清楚"是**状态**而不只是一次调用：轮询每 10 秒重算一次计划，
   * 没有这份记录就会每次都向 SFU 重发一遍"我要高画质"。
   */
  const subscriptionQualities = ref<Record<string, ScreenQuality>>({})

  /**
   * 已建立的**摄像头**订阅：sessionId → CameraSubscription（§24）。
   *
   * 与 `subscriptions` 分开两张表，理由与适配层完全相同：同一 participant 的
   * 两条轨道生命周期互相独立，合表之后任何一次退订都会顺手把另一条拆掉。
   */
  const cameraSubscriptions = shallowRef<Record<string, CameraSubscription>>({})

  /**
   * 摄像头订阅的媒体状态（键同样是 identity）。
   *
   * WHY 不复用 `mediaStates`（那是屏幕的）：一张卡片上"屏幕没有画面"与
   * "摄像头没有画面"是两个独立的结论，共用一个值会让老师看到
   * "画面订阅失败"出现在一个屏幕好好的卡片上。
   */
  const cameraMediaStates = ref<Record<string, MonitorMediaState>>({})

  /**
   * 已建立的**麦克风**订阅：sessionId → MicrophoneSubscription（§32）。
   *
   * 第三张表。§32 的核心不变量是"网格里绝不同时开多路音频"，而这条不变量由
   * **计划**保证（`desiredMicrophoneSubscriptions()` 最多返回一个 identity），
   * 不是因为这里只存得下一个。
   */
  const microphoneSubscriptions = shallowRef<Record<string, MicrophoneSubscription>>({})

  /** 麦克风订阅的媒体状态（与屏幕/摄像头各记一份，避免互相污染）。 */
  const microphoneMediaStates = ref<Record<string, MonitorMediaState>>({})

  /* ---------------------------------------------------------------------- */
  /* 老师自己的麦克风与私密语音（§27 / §31）                                  */
  /* ---------------------------------------------------------------------- */

  /** 老师麦克风的状态机（与学生端同形：off / requesting / on / error）。 */
  const teacherMicState = ref<TeacherMicState>('off')
  /** 老师麦克风失败的中文文案（来自 teacher-mic.ts 的词表，含下一步动作）。 */
  const teacherMicFailure = ref<string | null>(null)

  /**
   * 当前私密语音目标（§31：同一时刻只有一个）。
   *
   * 它是**服务端状态**的镜像：只会被 POST/GET 的响应、以及
   * `PRIVATE_TALK_STARTED` / `PRIVATE_TALK_ENDED` 事件改写。
   * 绝不从 `MonitorStudent` 推断——见 private-talk-api.ts 的说明。
   */
  const talkTarget = ref<PrivateTalkTarget | null>(null)
  /** 正在发请求（按钮 loading；防止老师连点导致两次 POST）。 */
  const talkBusy = ref(false)
  /** 上一次私密语音操作的失败（成功时清空）。 */
  const talkFailure = ref<PrivateTalkFailure | null>(null)

  /** 老师正在 Focus 看的学生（§30）；null = 在网格视图。 */
  const focusedStudentId = ref<string | null>(null)

  /** 页面是否处于不可见状态（老师切到了别的标签页）。 */
  const pageHidden = ref(false)

  /**
   * 实时通道状态（由 realtime store 转达）。
   *
   * 只用来决定兜底快照的频率；界面上的那句提示读的是 realtime store 本身。
   */
  const realtimeState = ref<RealtimeConnectionState>('closed')

  /**
   * 本课堂是否已经收到 `ROOM_CLOSED`（§49）。
   *
   * 单独一个字段而不是从学生状态推断：课堂结束与"学生都离开了"是两件事，
   * 界面在课堂结束时还要额外说明"画面已经释放，不需要重试媒体"。
   * 页面重新进入（load）时清零。
   */
  const classroomClosed = ref(false)

  /* ---------------------------------------------------------------------- */
  /* 只在内存里的私有内容                                                    */
  /* ---------------------------------------------------------------------- */

  /** 媒体凭据（§44）：闭包变量，不进 state、不写 URL、不进日志。 */
  let credentials: MediaCredentials | null = null
  let room: MonitorRoom | null = null
  let roomUnsubscribers: (() => void)[] = []
  /** 正在订阅中的 sessionId：第二次调用直接返回，不产生第二次 setSubscribed。 */
  const pending = new Set<string>()
  /**
   * 正在订阅摄像头中的 sessionId（与 `pending` 分开）。
   *
   * WHY 不能共用一个集合：同一个人的屏幕订阅与摄像头订阅是两次独立请求，
   * 共用一个集合会让"屏幕正在订阅"把摄像头那次请求挡掉——表现是画中画永远不出现，
   * 而屏幕上一切正常。
   */
  const pendingCamera = new Set<string>()
  /**
   * 正在订阅麦克风中的 sessionId（同样与另外两个集合分开）。
   *
   * WHY 三个集合：屏幕、摄像头、麦克风是三次互相独立的请求。共用一个集合会让
   * "屏幕正在订阅"把麦克风那次挡掉——表现是 Focus 面板上说"正在听"，耳机里没有声音。
   */
  const pendingMicrophone = new Set<string>()
  /**
   * 老师自己的麦克风采集。
   *
   * 与订阅表一样：持有的是浏览器宿主对象（MediaStreamTrack），放进响应式 state
   * 会被 Vue 深度代理。界面需要的只是 `teacherMicState`。
   */
  let teacherMicCapture: TeacherMicrophoneCapture | null = null
  /**
   * 老师麦克风请求序号。
   *
   * WHY：`getUserMedia` 与 publish 都是异步的，而老师可以在几百毫秒里点开又点关。
   * 没有序号的话，"关闭"之后才回来的那条轨道会覆盖关闭动作的结果——界面显示已关闭，
   * 而系统麦克风指示灯还亮着（而且他可能真的还在被那一个学生听见）。
   */
  let teacherMicSeq = 0
  /**
   * 当前在视口里的学生（studentId，不是 sessionId）。
   *
   * WHY 用 studentId 当键：sessionId 会随"学生重连是否复用同一 Session"变化（§12），
   * 而可见性是**卡片**的属性，卡片是按 studentId 渲染的。用 sessionId 做键会在
   * 一次重连后丢掉整张卡片的可见性，表现为"滚动一下才重新出画面"。
   *
   * WHY 不是响应式：没有任何界面元素依赖它（不可见的卡片本来就不在屏幕上），
   * 它只是订阅计划的输入。做成 ref 只会让每次滚动都触发一轮组件重渲染。
   */
  const visibleStudentIds = new Set<string>()
  let pollTimer: ReturnType<typeof setInterval> | null = null
  /** 当前兜底定时器的间隔，用来判断"通道状态变了要不要重建定时器"。 */
  let pollIntervalMs = 0
  /** 合并事件风暴用的快照刷新定时器（见 scheduleSnapshotRefresh）。 */
  let snapshotTimer: ReturnType<typeof setTimeout> | null = null
  /**
   * 已应用的事件数量。
   *
   * WHY 需要它：快照请求是**异步**的，一份在事件到达**之前**发出、在事件之后才回来的
   * 响应，反映的是更早的时刻。用它去覆盖状态，就会把刚收到的事件结论（"这个人已经
   * 下线了"）又改回旧的，老师看到的状态回滚一次（§51 最忌讳的那种"界面撒谎"）。
   * `refresh()` 因此记住发起时的代数，回来时对不上就丢弃并安排重取。
   */
  let eventGeneration = 0
  let currentClassroomId: string | null = null
  /** 请求序号：晚发出的响应才能写状态。 */
  let loadSeq = 0
  /** 正在跑的那一轮协调；非 null 时新的请求只标记"跑完再来一轮"。 */
  let reconcileInFlight: Promise<void> | null = null
  let reconcileQueued = false

  const isEmpty = computed(() => loaded.value && students.value.length === 0)
  /** 已建立的订阅数（页头"正在监督 N 路画面"）。 */
  const subscribedCount = computed(() => Object.keys(subscriptions.value).length)
  /** 名单总数 M：Phase 7 起后端返回课堂全部被授权学生。 */
  const rosterCount = computed(() => students.value.length)
  /**
   * 已进入 N：有会话且在线的学生数。
   *
   * 与徽章用**同一个判据**（`isStudentEntered`）：界面上的"已进入 4"必须正好等于
   * 徽章为 🟢/🔴 的卡片数，否则老师会看到自己和自己矛盾的两个数字。
   */
  const enteredCount = computed(() => students.value.filter(isStudentEntered).length)
  /** 正在 Focus 的学生（null = 网格视图）；学生从名单消失时会被清掉。 */
  const focusedStudent = computed(
    () => students.value.find((student) => student.studentId === focusedStudentId.value) ?? null,
  )

  /** 每个学生的徽章（视图直接读，避免在模板里散落判断逻辑）。 */
  function badgeOf(student: MonitorStudent): MonitorBadge {
    return describeMonitorBadge(student)
  }

  /** 每个学生的派生展示状态（§29；徽章与卡片主体都从它派生）。 */
  function tileStateOf(student: MonitorStudent): MonitorTileState {
    return deriveMonitorTileState(student)
  }

  /** 某个学生的媒体订阅状态（没有记录时是 'none'）。 */
  function mediaStateOf(student: MonitorStudent): MonitorMediaState {
    if (student.sessionId === null) return 'none'
    return mediaStates.value[student.sessionId] ?? 'none'
  }

  /** 某个学生是否已经拿到了可播放的订阅。 */
  function subscriptionOf(student: MonitorStudent): ScreenSubscription | null {
    if (student.sessionId === null) return null
    return subscriptions.value[student.sessionId] ?? null
  }

  /** 某个学生当前被订阅的画质档（没有订阅时是 null）。 */
  function qualityOf(student: MonitorStudent): ScreenQuality | null {
    if (student.sessionId === null) return null
    return subscriptionQualities.value[student.sessionId] ?? null
  }

  /**
   * 某个学生的摄像头订阅（§24）；没有订阅时是 null。
   *
   * 视图拿它决定画不画画中画：**null 就不画**（见 camera-pip.ts 的说明），
   * 绝不渲染一个空白小窗。
   */
  function cameraSubscriptionOf(student: MonitorStudent): CameraSubscription | null {
    if (student.sessionId === null) return null
    return cameraSubscriptions.value[student.sessionId] ?? null
  }

  /** 某个学生摄像头订阅的媒体状态（没有记录时是 'none'）。 */
  function cameraMediaStateOf(student: MonitorStudent): MonitorMediaState {
    if (student.sessionId === null) return 'none'
    return cameraMediaStates.value[student.sessionId] ?? 'none'
  }

  /**
   * 某个学生的麦克风订阅（§32）；没有订阅时是 null。
   *
   * 视图**必须**用它来说"正在听谁的麦克风"：音频是看不见的，界面上不写出来，
   * 老师就没有任何办法知道自己的耳机里正在放什么（以及是不是正在被别人听见）。
   */
  function microphoneSubscriptionOf(student: MonitorStudent): MicrophoneSubscription | null {
    if (student.sessionId === null) return null
    return microphoneSubscriptions.value[student.sessionId] ?? null
  }

  /** 某个学生麦克风订阅的媒体状态（没有记录时是 'none'）。 */
  function microphoneMediaStateOf(student: MonitorStudent): MonitorMediaState {
    if (student.sessionId === null) return 'none'
    return microphoneMediaStates.value[student.sessionId] ?? 'none'
  }

  /**
   * 老师**现在**是不是正在听这个学生的麦克风。
   *
   * 判据是"有订阅"而不是"DTO 说他开着麦"：两者在切换的那几秒里会不一致，
   * 而老师需要的是"我耳机里现在真的有他的声音吗"这个事实。
   */
  function isListeningToMicrophone(student: MonitorStudent): boolean {
    return microphoneSubscriptionOf(student) !== null
  }

  /* ---------------------------------------------------------------------- */
  /* 老师自己的麦克风（§27 / §31）                                           */
  /* ---------------------------------------------------------------------- */

  const isTeacherMicOn = computed(() => teacherMicState.value === 'on')
  const isTeacherMicRequesting = computed(() => teacherMicState.value === 'requesting')
  const teacherMicActionLabel = computed(() =>
    isTeacherMicOn.value ? '关闭麦克风' : '🎤 开启麦克风',
  )

  /**
   * 释放老师麦克风采集（幂等）。
   *
   * WHY 必须真的 `stop()`：它是唯一能让**操作系统级**麦克风指示灯灭掉的动作。
   * 老师点了"关闭麦克风"而灯还亮着，等于让他在不知情的情况下继续被采集。
   */
  function releaseTeacherMic(): void {
    releaseTeacherMicrophone(teacherMicCapture)
    teacherMicCapture = null
  }

  /** 连接已经不可用时丢弃老师麦克风（不试图 unpublish：没有信令通道）。 */
  function dropTeacherMicrophone(): void {
    teacherMicSeq += 1
    releaseTeacherMic()
    teacherMicState.value = 'off'
    teacherMicFailure.value = null
  }

  /**
   * 开启老师麦克风（§27/§31）。
   *
   * 顺序与音频订阅无关，只有一条纪律：**先申请设备、再发布**。反过来的话，
   * publish 成功而设备授权失败，服务端会先看到一条麦克风发布再消失。
   *
   * 这个函数**从不抛出**：失败写在 `teacherMicState` / `teacherMicFailure` 里，
   * 由界面显示一句可执行的话。
   */
  async function enableTeacherMicrophone(): Promise<void> {
    if (teacherMicState.value === 'requesting' || teacherMicState.value === 'on') return
    const active = room
    /**
     * 没有媒体连接时不申请设备权限：一条没有房间可发布的轨道只会亮着麦克风指示灯，
     * 而谁也听不到（§31 的语音沟通根本建立不起来）。
     */
    if (active === null) return

    const seq = ++teacherMicSeq
    teacherMicState.value = 'requesting'
    teacherMicFailure.value = null
    const abandoned = (): boolean => seq !== teacherMicSeq || room !== active

    try {
      const next = await requestTeacherMicrophone()
      if (abandoned()) {
        // 请求飞行期间被放弃：这条轨道没有主人了，必须立刻释放（灯必须灭）。
        releaseTeacherMicrophone(next)
        return
      }
      teacherMicCapture = next
      next.onEnded(() => {
        if (teacherMicCapture !== next) return
        /**
         * 设备被拔出 / 被别的程序抢走：界面必须回到"没开"，并撤下那条已经死掉的发布
         * （否则服务端会一直认为老师的麦克风还在，`TEACHER_MIC_REQUIRED` 也不会再出现）。
         */
        releaseTeacherMic()
        void active.unpublishMicrophoneTrack().catch(() => undefined)
        teacherMicState.value = 'error'
        teacherMicFailure.value = describeTeacherMicFailure('track-ended')
      })

      await active.publishMicrophoneTrack(next.track)
      if (abandoned()) {
        await active.unpublishMicrophoneTrack().catch(() => undefined)
        return
      }
      teacherMicState.value = 'on'
      /**
       * 开麦成功之后，上一次那条"请先开启你的麦克风"的提示已经不成立了：
       * 继续把它留在 Focus 面板上，老师会以为还需要再点一次。
       */
      if (talkFailure.value?.micRequired) talkFailure.value = null
    } catch (cause) {
      if (abandoned()) return
      releaseTeacherMic()
      teacherMicState.value = 'error'
      teacherMicFailure.value = describeTeacherMicFailure(toTeacherMicFailureKindOf(cause))
    }
  }

  /**
   * 关闭老师麦克风：**unpublish + 真正 stop**。
   *
   * 两步都要做，理由与学生端完全一致（只 unpublish 灯不灭；只 stop 服务端留着
   * 一条没有声音的发布）。先发信令再释放设备：老师点了"关闭"，录音必须立刻停。
   */
  function disableTeacherMicrophone(): void {
    if (teacherMicState.value === 'off' && teacherMicCapture === null) return
    teacherMicSeq += 1
    const active = room
    if (active !== null) void active.unpublishMicrophoneTrack().catch(() => undefined)
    releaseTeacherMic()
    teacherMicState.value = 'off'
    teacherMicFailure.value = null
  }

  /** 监督墙上的麦克风开关入口。 */
  function toggleTeacherMicrophone(): void {
    if (teacherMicState.value === 'on') disableTeacherMicrophone()
    else void enableTeacherMicrophone()
  }

  /* ---------------------------------------------------------------------- */
  /* 私密语音（§31）                                                         */
  /* ---------------------------------------------------------------------- */

  /**
   * 发起（或切换）与某个学生的私密语音（§31）。
   *
   * 切换**不需要**先结束：后端在设置新目标之前会撤销旧的订阅（§31 的"切换"），
   * 前端多发一次 DELETE 只会在中间留下一个"没有目标"的窗口——那个窗口里老师的
   * 麦克风可能被整个房间听到，而这正是本 Phase 最不能出现的情况。
   *
   * 未进入课堂的学生在这里直接拒绝（`canStartPrivateTalk`）：让老师点一个注定
   * 失败的按钮，等于把产品事实变成一次故障提示。
   */
  async function startTalk(student: MonitorStudent): Promise<void> {
    const classroomId = currentClassroomId
    if (classroomId === null || talkBusy.value) return
    if (!canStartPrivateTalk(student)) return
    talkBusy.value = true
    talkFailure.value = null
    try {
      const target = await startPrivateTalk(classroomId, student.studentId)
      /**
       * 响应里的 `target` 就是权威答案。理论上它一定非空（POST 成功 ⇒ 有目标），
       * 但契约允许 null，所以这里按"没有目标"处理并给一句解释，而不是留一个
       * 半信半疑的高亮状态。
       */
      talkTarget.value = target
      if (target === null) talkFailure.value = toPrivateTalkFailure(new Error('empty-target'))
    } catch (cause) {
      talkFailure.value = toPrivateTalkFailure(cause)
    } finally {
      talkBusy.value = false
    }
  }

  /** 结束私密语音（§31 的 TALKING → IDLE）。 */
  async function stopTalk(): Promise<void> {
    const classroomId = currentClassroomId
    if (classroomId === null || talkBusy.value) return
    talkBusy.value = true
    talkFailure.value = null
    try {
      await stopPrivateTalk(classroomId)
      talkTarget.value = null
    } catch (cause) {
      talkFailure.value = toPrivateTalkFailure(cause)
    } finally {
      talkBusy.value = false
    }
  }

  /**
   * 读取服务端当前目标（页面加载时）。
   *
   * WHY 必须读：私密语音是服务端状态。老师刷新页面、或在另一个标签页里开过语音沟通，
   * 不读一次就会出现"声音正在发出去，而界面上的按钮写着「语音沟通」"。
   * 读取失败**不改变**界面（保持无目标），也不报错：它只是一次恢复尝试，
   * 真正的目标会在下一次 `PRIVATE_TALK_STARTED` 或 `refresh()` 里对齐。
   */
  async function restoreTalkTarget(classroomId: string): Promise<void> {
    try {
      const target = await getPrivateTalk(classroomId)
      /**
       * 服务端说有个目标，而名单里没有这个人：他是被移出课堂之后才结束的。
       * 这种情况下界面必须回到"无目标"——显示一个不在名单里的人只会让老师困惑。
       */
      talkTarget.value =
        target !== null && students.value.some((student) => student.studentId === target.studentId)
          ? target
          : null
    } catch {
      // 见上：恢复失败保持"无目标"。
    }
  }

  /** 清掉私密语音的界面状态（课堂结束、媒体断开、离开页面）。 */
  function clearTalk(): void {
    talkTarget.value = null
    talkFailure.value = null
    talkBusy.value = false
  }

  /** 某个学生是不是当前私密语音目标（Focus 面板与卡片高亮都用它）。 */
  function isTalkTarget(student: MonitorStudent): boolean {
    return talkTarget.value?.studentId === student.studentId
  }

  /* ---------------------------------------------------------------------- */
  /* 订阅计划的输入（§52）                                                   */
  /* ---------------------------------------------------------------------- */

  /**
   * 卡片进入/离开视口。
   *
   * 这是 §52 的核心输入：不可见的卡片不产生下行。老师滚动一次，就有一批订阅
   * 被建立、另一批被撤销——这正是"不要同时下载 30 路画面"的落地方式。
   */
  function setStudentVisible(studentId: string, visible: boolean): void {
    if (visible) visibleStudentIds.add(studentId)
    else visibleStudentIds.delete(studentId)
    void reconcileSubscriptions()
  }

  /**
   * 进入 / 退出 Focus（§30）。
   *
   * Focus 的学生即使卡片被大画面盖住也要订阅（老师的注意力就在他身上），
   * 并且用高画质；其他学生**不动**——他们还留在视口里，撤销他们只会让
   * 关闭 Focus 后重新拉一遍流。
   */
  function setFocusedStudent(studentId: string | null): void {
    focusedStudentId.value = studentId
    /**
     * 换一个学生看时清掉上一次的私密语音失败提示：那条错误说的是**上一个人**
     * 发生了什么（"他不在课堂里"），留在新面板上会让老师以为这个学生也有问题。
     */
    talkFailure.value = null
    void reconcileSubscriptions()
  }

  /**
   * 页面可见性变化（老师切标签页）。
   *
   * WHY 老师端可以这么做，学生端不行（§23）：学生端的职责是"持续共享整块屏幕"，
   * 老师随时可能在看，所以学生端**不能**因为自己页面不可见就改变行为——
   * Chrome 最小化不是异常。老师端是纯粹的接收方，页面不可见时那些像素没有任何人看，
   * 继续下行只是白烧带宽，所以不可见即停、回到页面再恢复。
   */
  function setPageHidden(hidden: boolean): void {
    if (pageHidden.value === hidden) return
    pageHidden.value = hidden
    void reconcileSubscriptions()
  }

  /* ---------------------------------------------------------------------- */
  /* 媒体连接                                                                */
  /* ---------------------------------------------------------------------- */

  function detachRoomListeners(): void {
    for (const off of roomUnsubscribers) off()
    roomUnsubscribers = []
  }

  /**
   * 订阅房间事件。
   *
   * 这些事件只更新**媒体状态**（画面到没到、连接还在不在），绝不改业务状态：
   * 学生是否还在上课由 monitor 轮询说了算（§51）。
   */
  function subscribeRoomEvents(active: MonitorRoom): void {
    roomUnsubscribers.push(
      active.onParticipantsChanged(() => {
        // 有人加入/离开、或屏幕轨道发布/取消：让"该订阅的人"与"已订阅的人"重新对齐。
        // 业务状态一律不动——那是 monitor DTO 的事（§51）。
        void syncWithMediaParticipants()
      }),
      active.onScreenSubscribed((identity) => {
        mediaStates.value = { ...mediaStates.value, [identity]: 'subscribed' }
      }),
      active.onScreenUnsubscribed((identity) => {
        dropLocalSubscription(identity)
      }),
      active.onDisconnected(() => {
        // 媒体面断了：整体标红并给重试入口。业务状态保持不动——
        // 学生还在不在课堂上不是 LiveKit 能回答的问题（§51）。
        mediaConnected.value = false
        mediaError.value = '与课堂的媒体连接已经中断。请重试；重试会重新申请一次媒体凭据。'
        /**
         * 老师的麦克风必须在这里释放（§31）：连接没了，`on` 就不再成立
         * （"已发布、学生听得到"），而且设备不能被后台占着。
         * 那段私密语音也随之失效——继续显示"正在与张三语音沟通"就是撒谎。
         */
        dropTeacherMicrophone()
        clearTalk()
        // 刻意**不**停掉兜底快照：画面断了不代表"谁在上课"这件事不再重要，
        // 而且它与媒体面完全独立（§33 的 Control Plane / Media Plane 分离）。
      }),
    )
  }

  /**
   * 媒体层说"这个人已经不在房间里了"时，立刻丢掉本地订阅。
   *
   * WHY 不能等下一次 monitor 轮询（最多 10 秒）：那 10 秒里卡片会停着**最后一帧画面**，
   * 老师会以为学生还在共享。§51 把"画面是不是真的到了"交给媒体层，
   * 媒体层说人走了，就必须马上反映出来。
   */
  async function syncWithMediaParticipants(): Promise<void> {
    const active = room
    if (active === null) return
    const present = new Set(active.participants().map((participant) => participant.identity))
    for (const identity of Object.keys(subscriptions.value)) {
      if (present.has(identity)) continue
      dropLocalSubscription(identity)
      // 适配层里对已经离开的 participant 是空操作；这里调用是为了对称与幂等。
      await active.unsubscribeScreen(identity)
    }
    /**
     * 摄像头那一半同样要跟着释放（§24）。
     *
     * WHY 不能只靠协调计划：计划是按 monitor DTO 算的，而 DTO 最多 60 秒才刷一次。
     * 那 60 秒里右下角会停着**最后一帧**画面——老师会以为学生还在摄像头前，
     * 而人早就走了。媒体层说人没了，就必须立刻反映出来。
     */
    for (const identity of Object.keys(cameraSubscriptions.value)) {
      if (present.has(identity)) continue
      dropLocalCameraSubscription(identity)
      await active.unsubscribeCamera(identity)
    }
    /**
     * 麦克风那一半同样（§32）。
     *
     * WHY 这一条比另外两条更急：音频**看不见**。学生已经离开、而订阅还挂在本地时，
     * Focus 面板上仍然写着"正在听该学生的麦克风"——老师会以为设备坏了，
     * 而实际上对面早就没人了。媒体层说人没了，界面上的这句话必须立刻消失。
     */
    for (const identity of Object.keys(microphoneSubscriptions.value)) {
      if (present.has(identity)) continue
      dropLocalMicrophoneSubscription(identity)
      await active.unsubscribeMicrophone(identity)
    }
    await reconcileSubscriptions()
  }

  /** 只清本地记录（订阅对象、画质、媒体状态）；不碰连接。 */
  function dropLocalSubscription(identity: string): void {
    pending.delete(identity)
    subscriptions.value = omitKey(subscriptions.value, identity)
    subscriptionQualities.value = omitKey(subscriptionQualities.value, identity)
    mediaStates.value = { ...mediaStates.value, [identity]: 'none' }
  }

  /** 摄像头那一半：只清摄像头的记录，屏幕的订阅一行都不动（§24）。 */
  function dropLocalCameraSubscription(identity: string): void {
    pendingCamera.delete(identity)
    cameraSubscriptions.value = omitKey(cameraSubscriptions.value, identity)
    cameraMediaStates.value = { ...cameraMediaStates.value, [identity]: 'none' }
  }

  /** 申请凭据并连接（幂等：已经连上就直接返回 true）。 */
  async function ensureMediaConnected(classroomId: string): Promise<boolean> {
    if (room !== null && credentials !== null) return true
    try {
      const token = await requestMediaToken(classroomId)
      credentials = { livekitUrl: token.livekitUrl, token: token.token }
      const next = await createMonitorRoom(credentials)
      subscribeRoomEvents(next)
      await next.connect()
      room = next
      mediaConnected.value = true
      mediaError.value = null
      return true
    } catch {
      // 失败原因不打印：异常对象可能带上地址，甚至凭据片段（§44）。
      room = null
      credentials = null
      mediaConnected.value = false
      mediaError.value = '无法连接课堂的媒体服务器。请检查网络后重试。'
      return false
    }
  }

  /* ---------------------------------------------------------------------- */
  /* 订阅协调（§52 的手动订阅 + 可见性驱动）                                  */
  /* ---------------------------------------------------------------------- */

  /**
   * 现在**该**订阅谁、各自什么画质（§52 的策略只有这一处）。
   *
   * 三道筛子，顺序即优先级：
   * 1. 页面不可见 → 一路都不订。老师看不到任何像素，下行全是浪费；
   * 2. 业务上不该有画面的（未进入 / 连接中 / 已断开 / 已离开 / 屏幕中断）
   *    → 不订，也不保留订阅（§52 的"断开卡片不留订阅"）；
   * 3. 不在视口里且不是 Focus 对象 → 不订。
   *
   * Focus 与可见性是"或"的关系：Focus 的大画面会盖住网格，被盖住的卡片
   * 在 IntersectionObserver 眼里仍然算相交（它只看几何），但我们不依赖这一点——
   * 明确把 Focus 对象列进来，"焦点永远有画面"就不会被别处的改动破坏。
   */
  function desiredSubscriptions(): Map<string, ScreenQuality> {
    const desired = new Map<string, ScreenQuality>()
    if (pageHidden.value) return desired
    for (const student of students.value) {
      if (!shouldSubscribeScreen(student)) continue
      const identity = student.sessionId
      // shouldSubscribeScreen 已经保证非空；这里是为了把类型收窄成 string。
      if (identity === null) continue
      const focused = student.studentId === focusedStudentId.value
      if (!focused && !visibleStudentIds.has(student.studentId)) continue
      desired.set(identity, focused ? FOCUS_QUALITY : GRID_QUALITY)
    }
    return desired
  }

  /**
   * 现在**该**订阅谁的摄像头（§24 / §29）。
   *
   * 与屏幕那份计划共用三路输入（可见性 / Focus / 页面隐藏），但**筛的是另一个字段**
   * （`camera.active` 而不是 `screen.active`）：摄像头是 optional（§21），
   * 屏幕在不在发布与摄像头开没开没有任何推导关系。
   *
   * 摄像头**不区分 Focus**：画中画与 Focus 右侧的 Camera 区都是小窗，
   * 所以它只有"订 / 不订"两种状态，没有画质档（§52 的分层只针对屏幕）。
   */
  function desiredCameraSubscriptions(): Set<string> {
    const desired = new Set<string>()
    if (pageHidden.value) return desired
    for (const student of students.value) {
      if (!shouldSubscribeCamera(student)) continue
      const identity = student.sessionId
      if (identity === null) continue
      const focused = student.studentId === focusedStudentId.value
      if (!focused && !visibleStudentIds.has(student.studentId)) continue
      desired.add(identity)
    }
    return desired
  }

  /**
   * 现在**该**听谁的麦克风（§32）。
   *
   * 返回类型是 `string | null` 而不是集合，这是本函数存在的全部意义：
   * §32 禁止网格里同时开多路音频——多路混在一起之后，老师分辨不出是谁在说话，
   * 这条能力本身也就失去了价值。用 `Set` 会让"最多一个"变成一句口头约定，
   * 而用单值让它在类型层面成立。
   *
   * 三道筛子，顺序即优先级：
   * 1. 页面不可见 → 不订（老师切走了，没人听）；
   * 2. 不是 Focus 对象 → 不订。§32 要的是"当前 Focus 学生的麦克风"，
   *    而不是"每个可见卡片的麦克风"——后者正是要避免的叠音；
   * 3. 业务上麦克风没开（或学生不在线）→ 不订（`shouldSubscribeMicrophone`）。
   */
  function desiredMicrophoneIdentity(): string | null {
    if (pageHidden.value) return null
    const focused = students.value.find(isFocusedStudent)
    if (!focused || !shouldSubscribeMicrophone(focused)) return null
    return focused.sessionId
  }

  /**
   * 协调入口：三路输入（可见性 / Focus / 页面隐藏）与轮询都调它。
   *
   * 串行化的理由见文件头第 6 点。重复调用是**廉价且安全**的：
   * 绝大多数情况下这一轮什么都不会做（计划与现状一致），这也是它敢被
   * 每次滚动、每次轮询调用的前提。
   */
  function reconcileSubscriptions(): Promise<void> {
    if (reconcileInFlight !== null) {
      // 已经有一轮在跑：标记"跑完再算一次"。晚到的输入（滚动 / Focus / hidden）
      // 正是靠这一句生效，否则它们会被静默丢掉。
      reconcileQueued = true
      return reconcileInFlight
    }
    reconcileInFlight = runReconcileLoop()
    return reconcileInFlight
  }

  async function runReconcileLoop(): Promise<void> {
    try {
      do {
        reconcileQueued = false
        await applySubscriptionPlan()
      } while (reconcileQueued)
    } finally {
      reconcileInFlight = null
    }
  }

  /**
   * 把"现状"收敛到"计划"。
   *
   * 先退订不再需要的，再补订缺的；已经订着且仍然需要的，只调整画质。
   * 两趟都必须幂等：轮询每 10 秒就会走一遍。
   */
  async function applySubscriptionPlan(): Promise<void> {
    const active = room
    if (active === null) return

    const desired = desiredSubscriptions()

    for (const identity of Object.keys(subscriptions.value)) {
      if (desired.has(identity)) continue
      dropLocalSubscription(identity)
      // 取消订阅（而不是只停止播放）：让服务端不再往下推，这才是省带宽的那一步（§52）。
      await active.unsubscribeScreen(identity)
    }

    /**
     * 补订**并发**发出，而不是一个个 await。
     *
     * WHY：`subscribeScreen` 要等到轨道真的推下来才 resolve（可能几百毫秒，
     * 慢的时候到超时上限）。串行 await 会让第 N 张卡片等到前面 N-1 条轨道全部到位
     * 才开始请求——一格 20 人的监督墙就要花十几秒才能填满，而且这十几秒里
     * 老师看到的是"正在订阅画面…"，会以为系统坏了。
     * 订阅请求本身是各自独立的，没有任何顺序依赖。
     */
    await Promise.all(
      [...desired].map(([identity, quality]) => establishSubscription(active, identity, quality)),
    )

    /**
     * 摄像头走**同一轮**协调（§52 的"不要另起一套"）。
     *
     * WHY 必须挂在同一个循环里：可见性、Focus、页面隐藏三路输入已经在上面合成过一遍
     * 计划了，摄像头另起一条链路就会出现"滚动时屏幕退了、摄像头还留着"这种
     * 只在真机上偶发的状态。这里只是把同一个计划里的另一半收敛掉。
     */
    await convergeCameraSubscriptions(active)

    /**
     * 麦克风也走**同一轮**协调（§32）。
     *
     * WHY 同一条理由：三路输入（可见性 / Focus / 页面隐藏）已经在上面合成过一遍计划，
     * 音频另起一条链路就会出现"退出 Focus 了、耳机里还在放那个学生"这种
     * 只会在真机上被发现的状态。而且它必须在**同一次**收敛里完成，
     * 否则"最多一路音频"这条不变量会在两次协调之间被短暂破坏。
     */
    await convergeMicrophoneSubscription(active)
  }

  /**
   * 麦克风订阅的收敛：**至多一条**。
   *
   * 先退掉所有不是当前应听对象的订阅，再补上那一个。因为计划是单值，
   * 这个循环最多退掉一条、最多补上一条——"不会同时听到两个人"因此在代码结构上成立，
   * 而不是靠调用方记得先退再订。
   */
  async function convergeMicrophoneSubscription(active: MonitorRoom): Promise<void> {
    const desired = desiredMicrophoneIdentity()

    for (const identity of Object.keys(microphoneSubscriptions.value)) {
      if (identity === desired) continue
      dropLocalMicrophoneSubscription(identity)
      await active.unsubscribeMicrophone(identity)
    }

    if (desired !== null) await establishMicrophoneSubscription(active, desired)
  }

  /**
   * 建立一条麦克风订阅并把结果写回状态（失败只影响"能不能听到"）。
   *
   * 与屏幕/摄像头一样的幂等 + pending 双保险：Focus 每 10 秒一轮的刷新、
   * `MIC_CHANGED` 事件、轨道发布事件都会重算计划。
   */
  async function establishMicrophoneSubscription(
    active: MonitorRoom,
    identity: string,
  ): Promise<void> {
    if (microphoneSubscriptions.value[identity] !== undefined) return
    if (pendingMicrophone.has(identity)) return
    pendingMicrophone.add(identity)
    microphoneMediaStates.value = { ...microphoneMediaStates.value, [identity]: 'pending' }
    try {
      const subscription = await active.subscribeMicrophone(identity)
      if (subscription === null) {
        /**
         * 业务说他开着麦，媒体里却还没有这条轨道：可能是刚发布（webhook 比 SFU 快）。
         * 保持 'none' 并等下一次协调——界面显示"正在连接该学生的麦克风…"，
         * 而屏幕/摄像头完全不受影响。轨道发布事件会立刻再触发一轮。
         */
        microphoneMediaStates.value = { ...microphoneMediaStates.value, [identity]: 'none' }
        return
      }
      microphoneSubscriptions.value = {
        ...microphoneSubscriptions.value,
        [identity]: markRaw(subscription),
      }
      microphoneMediaStates.value = { ...microphoneMediaStates.value, [identity]: 'subscribed' }
    } catch {
      microphoneMediaStates.value = { ...microphoneMediaStates.value, [identity]: 'failed' }
    } finally {
      pendingMicrophone.delete(identity)
    }
  }

  /** 只清麦克风的本地记录（订阅对象与媒体状态）；不碰屏幕与摄像头。 */
  function dropLocalMicrophoneSubscription(identity: string): void {
    pendingMicrophone.delete(identity)
    microphoneSubscriptions.value = omitKey(microphoneSubscriptions.value, identity)
    microphoneMediaStates.value = { ...microphoneMediaStates.value, [identity]: 'none' }
  }

  /**
   * 摄像头订阅的收敛（先退掉不该留的，再补上缺的）。
   *
   * 与屏幕那半的差别只有画质：摄像头没有档位（永远 LOW，见 camera-pip.ts）。
   */
  async function convergeCameraSubscriptions(active: MonitorRoom): Promise<void> {
    const desired = desiredCameraSubscriptions()

    for (const identity of Object.keys(cameraSubscriptions.value)) {
      if (desired.has(identity)) continue
      dropLocalCameraSubscription(identity)
      await active.unsubscribeCamera(identity)
    }

    await Promise.all([...desired].map((identity) => establishCameraSubscription(active, identity)))
  }

  /** 建立一条摄像头订阅并把结果写回状态（失败只影响这一格的小窗）。 */
  async function establishCameraSubscription(active: MonitorRoom, identity: string): Promise<void> {
    if (cameraSubscriptions.value[identity] !== undefined) return
    // 与屏幕那半同一个防重复机制：同一 identity 的请求还没回来时不再发第二次。
    if (pendingCamera.has(identity)) return
    pendingCamera.add(identity)
    cameraMediaStates.value = { ...cameraMediaStates.value, [identity]: 'pending' }
    try {
      const subscription = await active.subscribeCamera(identity)
      if (subscription === null) {
        /**
         * 业务说他开着摄像头，媒体里却还没有这条轨道：可能是刚发布（webhook 比 SFU 快），
         * 也可能老师端网络问题。**保持 'none' 并等下一次协调**——画中画不出现，
         * 但屏幕画面完全不受影响。轨道发布事件（TrackPublished）会立刻再触发一轮。
         */
        cameraMediaStates.value = { ...cameraMediaStates.value, [identity]: 'none' }
        return
      }
      cameraSubscriptions.value = {
        ...cameraSubscriptions.value,
        [identity]: markRaw(subscription),
      }
      cameraMediaStates.value = { ...cameraMediaStates.value, [identity]: 'subscribed' }
    } catch {
      cameraMediaStates.value = { ...cameraMediaStates.value, [identity]: 'failed' }
    } finally {
      pendingCamera.delete(identity)
    }
  }

  /** 建立一条订阅并把结果写回状态（失败只影响这一格）。 */
  async function establishSubscription(
    active: MonitorRoom,
    identity: string,
    quality: ScreenQuality,
  ): Promise<void> {
    const existing = subscriptions.value[identity]
    if (existing !== undefined) {
      applyQuality(identity, existing, quality)
      return
    }
    // pending 是第三层防线（前两层在适配层）：同一 identity 的订阅请求还没回来时，
    // 又一轮协调进来了，不能发出第二个 setSubscribed。
    if (pending.has(identity)) return
    pending.add(identity)
    mediaStates.value = { ...mediaStates.value, [identity]: 'pending' }
    try {
      const subscription = await active.subscribeScreen(identity, quality)
      if (subscription === null) {
        // 业务说他在共享，但媒体里还没有这条轨道：可能是刚发布、也可能后端状态超前。
        // 保持 'none'，下一次轮询/事件会再试一次。
        mediaStates.value = { ...mediaStates.value, [identity]: 'none' }
        return
      }
      subscriptions.value = { ...subscriptions.value, [identity]: markRaw(subscription) }
      subscriptionQualities.value = { ...subscriptionQualities.value, [identity]: quality }
      mediaStates.value = { ...mediaStates.value, [identity]: 'subscribed' }
    } catch {
      mediaStates.value = { ...mediaStates.value, [identity]: 'failed' }
    } finally {
      pending.delete(identity)
    }
  }

  /**
   * 调整一条已有订阅的画质（§30）。
   *
   * 只有档位真的变了才下发：轮询每 10 秒重算一次计划，没有这道判断就会变成
   * 每 10 秒给 SFU 发一次"我要高画质"。
   */
  function applyQuality(
    identity: string,
    subscription: ScreenSubscription,
    quality: ScreenQuality,
  ): void {
    if (subscriptionQualities.value[identity] === quality) return
    subscriptionQualities.value = { ...subscriptionQualities.value, [identity]: quality }
    subscription.setQuality(quality)
  }

  /** 不可变删除（避免直接改 shallowRef 里的对象）。 */
  function omitKey<T>(source: Record<string, T>, key: string): Record<string, T> {
    const next = { ...source }
    delete next[key]
    return next
  }

  /* ---------------------------------------------------------------------- */
  /* 轮询                                                                     */
  /* ---------------------------------------------------------------------- */

  function stopPolling(): void {
    if (pollTimer === null) return
    clearInterval(pollTimer)
    pollTimer = null
    pollIntervalMs = 0
  }

  function startPolling(): void {
    if (currentClassroomId === null) return
    pollIntervalMs = 0
    syncPollTimer()
  }

  /**
   * 按实时通道状态重建兜底定时器（幂等：间隔没变就什么都不做）。
   *
   * 页面隐藏时**继续**轮询：一次 GET /monitor 的成本可以忽略，而它让老师切回来时
   * 看到的人数、徽章都是新的（订阅是另一回事，见 desiredSubscriptions）。
   */
  function syncPollTimer(): void {
    if (currentClassroomId === null) return
    const interval = monitorFallbackPollMs(realtimeState.value)
    if (pollTimer !== null && pollIntervalMs === interval) return
    if (pollTimer !== null) clearInterval(pollTimer)
    pollIntervalMs = interval
    pollTimer = setInterval(() => {
      void refresh()
    }, interval)
  }

  /**
   * 实时通道状态变化（由 realtime store 转达）。
   *
   * 通道断了 → 收紧兜底频率：这段时间里快照是唯一的信息来源，
   * 60 秒一次的"监督数据"对正在盯屏的老师来说太旧了（见 monitorFallbackPollMs）。
   */
  function setRealtimeState(next: RealtimeConnectionState): void {
    if (realtimeState.value === next) return
    realtimeState.value = next
    if (pollTimer !== null) syncPollTimer()
  }

  /**
   * 安排一次（合并的）快照刷新。
   *
   * WHY 需要它：事件载荷是**增量**的，有几类情况只有快照能回答——
   * 事件里出现了名单外的学生（可能刚被加入课堂）、课堂刚刚关闭（最终状态）。
   * 直接每个事件都 refresh 会在"30 个人陆续进来"时打出 30 个请求，
   * 所以合并到一个窗口里（最后一次事件之后 500ms 才发）。
   */
  function scheduleSnapshotRefresh(): void {
    if (snapshotTimer !== null) return
    snapshotTimer = setTimeout(() => {
      snapshotTimer = null
      void refresh()
    }, SNAPSHOT_REFRESH_DEBOUNCE_MS)
  }

  /** 拉一次 monitor 数据并重新协调订阅（轮询与"重试"按钮共用）。 */
  async function refresh(): Promise<void> {
    const classroomId = currentClassroomId
    if (classroomId === null) return
    const seq = ++loadSeq
    loading.value = true
    try {
      const generation = eventGeneration
      const next = await getMonitor(classroomId)
      if (seq !== loadSeq) return
      if (generation !== eventGeneration) {
        /**
         * 这份快照是在某条事件之前取的：丢弃它，并重取一次。
         * 丢弃的代价只是一次多余的请求，覆盖的代价是老师看着一个已经变化的状态做判断。
         */
        scheduleSnapshotRefresh()
        return
      }
      students.value = next
      loaded.value = true
      error.value = null
      /**
       * 名单变了（学生被移出课堂 / 换了一节课）：Focus 指向的人可能已经不存在。
       * 不清掉的话，Focus 面板会继续显示一个"已经不在名单里"的学生，
       * 而它的订阅早就被计划撤掉了——这正是最典型的"界面撒谎"。
       */
      if (focusedStudentId.value !== null && !next.some(isFocusedStudent)) {
        focusedStudentId.value = null
      }
      /**
       * 私密语音目标可能已经不在名单里了（学生被移出课堂）。不清掉的话，
       * 页头会继续写着"正在与张三语音沟通"，而服务端的订阅目标早已不存在——
       * 老师会对着一个空目标讲话。名单的权威是这次快照。
       */
      if (talkTarget.value !== null && !next.some(isTalkTargetStudent)) {
        clearTalk()
      }
      await reconcileSubscriptions()
    } catch (cause) {
      if (seq !== loadSeq) return
      /**
       * 业务面失败**不清空**已有数据：把刚看到的画面抹掉，老师会以为学生都掉线了。
       * 保留上一轮数据 + 一句错误提示，比一片空白诚实得多。
       */
      error.value = isApiError(cause) ? cause : new ApiError({ code: 'INTERNAL', cause })
      /**
       * 会话/凭据失效（§44）：媒体连接已经没有意义，立刻释放，别让一条**已经不被
       * 授权**的下行留着。路由守卫会把老师送回登录页，这里只保证不留下悬挂的连接。
       */
      if (error.value.code === 'AUTH_REQUIRED') await teardownMedia()
    } finally {
      if (seq === loadSeq) loading.value = false
    }
  }

  /* ---------------------------------------------------------------------- */
  /* 实时事件 → 业务状态增量（§47 / §51）                                     */
  /* ---------------------------------------------------------------------- */

  /**
   * 应用一条实时事件。
   *
   * 三条纪律，每一条都对应一次"界面撒谎"的风险：
   *
   * 1. **只改事件能证明的字段**。事件载荷里没有 `connection` / `joinedAt`，
   *    就不去动它们——用默认值补出来的"正常"是假数据（§80）。
   * 2. **名单外的学生一律忽略并记日志**，绝不凭空造一张卡片（§26/§47：
   *    学生的存在与否只有快照说了算）。同时安排一次快照刷新，
   *    因为"事件里有这个人、名单里没有"最常见的原因就是名单刚刚变了。
   * 3. **订阅变化必须走协调器**（§52）：事件只改业务状态，之后调
   *    `reconcileSubscriptions()` 让"该订谁"重新收敛。绕过协调器直接订阅/退订，
   *    就会与可见性、Focus、页面隐藏三路输入打架。
   */
  function applyRealtimeEvent(event: RealtimeEvent): void {
    switch (event.type) {
      case 'ROOM_CLOSED':
        if (event.data.classroomId !== currentClassroomId) return
        onClassroomClosed()
        return
      case 'STUDENT_ONLINE': {
        const { studentId, sessionId, displayName } = event.data
        const changed = updateStudent(studentId, sessionId, 'STUDENT_ONLINE', (student) => ({
          ...student,
          displayName,
          sessionId,
          sessionStatus: 'ONLINE',
          /**
           * §21 的不变量：ONLINE ⇒ Screen Track 存在且在发布；§45 也要求后端
           * 只在 `track_published(source=SCREEN_SHARE)` 之后才把会话标成 ONLINE。
           * 因此这里把 screen.active 一起置真——不这样做的话，一个刚进入课堂的
           * 学生会瞬间显示成 🔴「屏幕中断」，那是对老师最严重的一种谎报。
           */
          screen: { active: true },
        }))
        if (changed) void reconcileSubscriptions()
        return
      }
      case 'STUDENT_OFFLINE': {
        const { studentId, sessionId, reason } = event.data
        const changed = updateStudent(studentId, sessionId, 'STUDENT_OFFLINE', (student) => ({
          ...student,
          sessionStatus: offlineStatus(reason),
          screen: { active: false },
          camera: { active: false },
          microphone: { active: false },
        }))
        /**
         * 目标学生离开 ⇒ 私密语音不可能继续（§31）。**不**等 `PRIVATE_TALK_ENDED`：
         * 那条事件会来，但"学生已经走了"这个事实此刻就成立，界面多显示一秒
         * "正在与张三语音沟通"就是多一秒的谎报。
         */
        if (talkTarget.value !== null && talkTarget.value.studentId === studentId) clearTalk()
        // 下线即释放订阅：协调计划会发现这个人不再满足 shouldSubscribeScreen（§52）。
        if (changed) void reconcileSubscriptions()
        return
      }
      case 'SCREEN_LOST': {
        const data = teacherScreenData(event.data)
        if (data === null) return
        const { studentId, sessionId } = data
        const changed = updateStudent(studentId, sessionId, 'SCREEN_LOST', (student) => ({
          ...student,
          sessionStatus: 'SCREEN_LOST',
          screen: { active: false },
        }))
        if (changed) void reconcileSubscriptions()
        return
      }
      case 'SCREEN_RESTORED': {
        const data = teacherScreenData(event.data)
        if (data === null) return
        const { studentId, sessionId } = data
        const changed = updateStudent(studentId, sessionId, 'SCREEN_RESTORED', (student) => ({
          ...student,
          sessionStatus: 'ONLINE',
          screen: { active: true },
        }))
        // 恢复后如果这张卡片在视口里（或正好是 Focus），协调器会把它订回来。
        if (changed) void reconcileSubscriptions()
        return
      }
      case 'CAMERA_CHANGED': {
        const { studentId, sessionId, active } = event.data
        /**
         * Phase 9：只改 `camera` 这一个字段。
         *
         * 事件证明不了别的——载荷里没有 `connection` / `joinedAt` / `sessionStatus`，
         * 顺手"补一个看起来合理"的值就是造数据（§80）。摄像头本身也不改变会话状态
         * （§21/§24：屏幕才是 mandatory），所以这里连 `sessionStatus` 都不碰。
         */
        const changed = updateStudent(studentId, sessionId, 'CAMERA_CHANGED', (student) => ({
          ...student,
          camera: { active },
        }))
        /**
         * 订阅要跟着变，但**仍然走协调器**（§52）：这里只重新收敛一次计划，
         * 直接调 subscribeCamera/unsubscribeCamera 就会与可见性、Focus、
         * 页面隐藏三路输入打架——那正是 Phase 7 建这套协调器的原因。
         */
        if (changed) void reconcileSubscriptions()
        return
      }
      case 'MIC_CHANGED': {
        const { studentId, sessionId, active } = event.data
        /**
         * Phase 10（§25/§32）：只改 `microphone` 这一个字段。
         *
         * 事件证明不了别的——载荷里没有 `connection` / `joinedAt` / `sessionStatus`，
         * 顺手"补一个看起来合理"的值就是造数据（§80）。麦克风也不改变会话状态
         * （§21：屏幕才是 mandatory），所以这里连 `sessionStatus` 都不碰。
         *
         * 之后走**协调器**：Focus 中的学生一开麦，最多几百毫秒后老师就该听到声音
         * （轨道发布事件也会触发同一轮协调）；而如果他关麦，协调器会把那一路音频退掉。
         * 直接调 subscribeMicrophone 就会与 Focus / 页面隐藏两路输入打架。
         */
        const changed = updateStudent(studentId, sessionId, 'MIC_CHANGED', (student) => ({
          ...student,
          microphone: { active },
        }))
        if (changed) void reconcileSubscriptions()
        return
      }
      case 'PRIVATE_TALK_STARTED': {
        /**
         * §31：后端确认了目标（可能是另一个标签页发起的，也可能是对本次 POST 的
         * 回执）。老师那版的载荷带身份三件套；收到学生版说明这条消息不该发给我。
         */
        if (!('studentId' in event.data)) return
        const { studentId, sessionId, displayName } = event.data
        talkTarget.value = { studentId, sessionId, displayName }
        talkFailure.value = null
        return
      }
      case 'PRIVATE_TALK_ENDED': {
        const { sessionId } = event.data
        // 按 sessionId 匹配当前目标：那是两端都有的那把键（§44）。
        if (talkTarget.value !== null && talkTarget.value.sessionId === sessionId) {
          talkTarget.value = null
        }
        return
      }
      default:
        return
    }
  }

  /**
   * 屏幕类事件在老师端**一定**带 `studentId`（§47 按角色给不同的 data 形状）。
   * 不带就说明这条载荷其实是发给学生本人的，老师端没有任何可做的事——
   * 宁可忽略，也不要用 `sessionId` 冒充 `studentId` 去猜一个人。
   */
  function teacherScreenData(data: ScreenStateData): TeacherScreenStateData | null {
    return 'studentId' in data ? data : null
  }

  /**
   * 事件 → 名单里的那一行。
   *
   * WHY 先按 sessionId 找、再按 studentId 找：`sessionId` 就是媒体层的 identity（§44），
   * 屏幕类事件天然带着它；而 `STUDENT_ONLINE` 可能带来一个**新的** sessionId
   * （学生重连后后端新建了 Session，§50），此时只能靠 studentId 认出是同一个人。
   *
   * 返回是否真的改动了名单——没改动时不必跑一遍订阅协调。
   */
  function updateStudent(
    studentId: string,
    sessionId: string,
    eventLabel: string,
    patch: (student: MonitorStudent) => MonitorStudent,
  ): boolean {
    const index = students.value.findIndex(
      (student) => student.sessionId === sessionId || student.studentId === studentId,
    )
    if (index < 0) {
      /**
       * 名单里没有这个人：**忽略并记日志**，绝不凭空造卡片（§47）。
       * 日志里只带事件类型，不带姓名/会话 id（§59 的日志纪律）。
       * 同时安排一次快照刷新——"事件里有人、名单里没有"通常意味着名单变了，
       * 而名单的权威只有 `GET monitor`。
       */
      console.warn(`[monitor] 忽略名单外学生的实时事件：${eventLabel}`)
      scheduleSnapshotRefresh()
      return false
    }
    students.value = students.value.map((student, current) =>
      current === index ? patch(student) : student,
    )
    eventGeneration += 1
    return true
  }

  /**
   * 课堂被老师关闭（§49 的老师侧）。
   *
   * 媒体必须**主动**释放：LiveKit 房间会被服务端终止，等着它自己断会先在界面上
   * 弹一句"无法看到学生画面"，而那是关课的正常结果，不是故障。
   * 随后补一次快照，把每个学生的最终状态（ROOM_CLOSED）取回来。
   */
  function onClassroomClosed(): void {
    classroomClosed.value = true
    eventGeneration += 1
    /**
     * 课堂结束了 ⇒ 私密语音不可能继续（§31 的 TALKING 一定是短暂的）。
     * 先清界面状态，再释放媒体：`teardownMedia` 是异步的，而界面上的
     * "正在与张三语音沟通"必须在这一 tick 里消失。
     */
    clearTalk()
    void teardownMedia()
    scheduleSnapshotRefresh()
  }

  /** §12 的离线原因 → 会话状态（两者一一对应，不做额外推断）。 */
  function offlineStatus(reason: StudentOfflineReason): StudentSessionStatus {
    switch (reason) {
      case 'DISCONNECTED':
        return 'DISCONNECTED'
      case 'LEFT':
        return 'LEFT'
      case 'ROOM_CLOSED':
        return 'ROOM_CLOSED'
    }
  }

  function isFocusedStudent(student: MonitorStudent): boolean {
    return student.studentId === focusedStudentId.value
  }

  /** 名单里是否还有当前私密语音目标那个人（快照兜底用）。 */
  function isTalkTargetStudent(student: MonitorStudent): boolean {
    return student.studentId === talkTarget.value?.studentId
  }

  /**
   * 进入监督墙：申请媒体凭据 → 连接 → 拉 monitor → 订阅 → 开始轮询。
   *
   * 两步失败的处理不同：媒体失败仍然要把 monitor 数据拉出来（老师至少能看到
   * "谁在上课"），业务失败也不影响已经建立的媒体连接。这两件事各自可重试。
   */
  async function load(classroomId: string): Promise<void> {
    currentClassroomId = classroomId
    classroomClosed.value = false
    await ensureMediaConnected(classroomId)
    await refresh()
    /**
     * 恢复"我正在和谁讲话"（§31）。**不 await**：这是一次恢复尝试，
     * 后端慢或不可达时绝不能拖住后面的 `startPolling()`——那会让监督墙
     * 连"谁在上课"都不再刷新，为了一句界面记忆付出过高的代价。
     * 它在 `refresh()` 之后发起，这样校验名单时用的是刚拿到的数据。
     */
    void restoreTalkTarget(classroomId)
    startPolling()
  }

  /** 媒体失败后的重试：重新申请凭据、重连、并把订阅全部重建（§52）。 */
  async function retryMedia(): Promise<void> {
    const classroomId = currentClassroomId
    if (classroomId === null) return
    mediaError.value = null
    await teardownMedia()
    await ensureMediaConnected(classroomId)
    await reconcileSubscriptions()
    // 断线期间可能正好被停掉了轮询：恢复它。
    startPolling()
  }

  /** 断开媒体并清空订阅（幂等）。 */
  async function teardownMedia(): Promise<void> {
    detachRoomListeners()
    const current = room
    room = null
    credentials = null
    mediaConnected.value = false
    subscriptions.value = {}
    subscriptionQualities.value = {}
    mediaStates.value = {}
    cameraSubscriptions.value = {}
    cameraMediaStates.value = {}
    microphoneSubscriptions.value = {}
    microphoneMediaStates.value = {}
    pending.clear()
    pendingCamera.clear()
    pendingMicrophone.clear()
    /**
     * 老师自己的麦克风必须跟着连接一起放手（§31）。
     *
     * WHY 把释放设备放在 `teardownMedia` 而不是各个调用点：它是"这条媒体连接结束了"
     * 的唯一出口（退出、重试、课堂关闭都走它）。分开写就一定会漏掉一条路径，
     * 而漏掉的后果是老师的麦克风指示灯在课堂结束后继续亮着。
     */
    dropTeacherMicrophone()
    if (current !== null) {
      try {
        await current.disconnect()
      } catch {
        // 已断开的连接再断一次会抛；退出路径不能被它打断。
      }
    }
  }

  /**
   * 离开页面：停止轮询、断开媒体、清空一切。
   *
   * 可见性与 Focus 也要清掉：store 的生命周期比页面长（Pinia 单例），
   * 留着上次的可见集合会让"重新进入监督墙"的第一帧就订阅一批根本不在视口里的卡片。
   */
  async function stop(): Promise<void> {
    stopPolling()
    if (snapshotTimer !== null) {
      clearTimeout(snapshotTimer)
      snapshotTimer = null
    }
    await teardownMedia()
    currentClassroomId = null
    students.value = []
    loaded.value = false
    error.value = null
    mediaError.value = null
    loading.value = false
    focusedStudentId.value = null
    pageHidden.value = false
    classroomClosed.value = false
    visibleStudentIds.clear()
    // 私密语音与老师麦克风的状态也要清：store 的生命周期比页面长（Pinia 单例），
    // 留着会让"重新进入监督墙"的第一帧就显示"正在与张三语音沟通"。
    clearTalk()
  }

  return {
    students,
    loading,
    loaded,
    isEmpty,
    error,
    mediaError,
    mediaConnected,
    mediaStates,
    subscriptions,
    subscriptionQualities,
    cameraSubscriptions,
    cameraMediaStates,
    microphoneSubscriptions,
    microphoneMediaStates,
    subscribedCount,
    rosterCount,
    enteredCount,
    focusedStudentId,
    focusedStudent,
    pageHidden,
    realtimeState,
    classroomClosed,
    teacherMicState,
    teacherMicFailure,
    isTeacherMicOn,
    isTeacherMicRequesting,
    teacherMicActionLabel,
    talkTarget,
    talkBusy,
    talkFailure,
    badgeOf,
    tileStateOf,
    mediaStateOf,
    subscriptionOf,
    qualityOf,
    cameraSubscriptionOf,
    cameraMediaStateOf,
    microphoneSubscriptionOf,
    microphoneMediaStateOf,
    isListeningToMicrophone,
    isTalkTarget,
    setStudentVisible,
    setFocusedStudent,
    setPageHidden,
    setRealtimeState,
    applyRealtimeEvent,
    enableTeacherMicrophone,
    disableTeacherMicrophone,
    toggleTeacherMicrophone,
    startTalk,
    stopTalk,
    clearTalk,
    load,
    refresh,
    retryMedia,
    reconcileSubscriptions,
    stop,
  }
})
