import { ApiError, isApiError } from '@classwatch/api-client'
import type { MonitorStudent, MonitorTileState } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, markRaw, ref, shallowRef } from 'vue'
import {
  createMonitorRoom,
  type MediaCredentials,
  type MonitorRoom,
  type ScreenQuality,
  type ScreenSubscription,
} from '../lib/media/media-room.ts'
import {
  deriveMonitorTileState,
  isStudentEntered,
  shouldSubscribeScreen,
  type MonitorBadge,
  type MonitorMediaState,
  describeMonitorBadge,
} from '../lib/monitor-status.ts'
import { getMonitor, requestMediaToken } from '../lib/teacher-monitor-api.ts'

/**
 * 老师端监督 store（§27 / §29 / §30 / §51 / §52）。
 *
 * 它把两路**完全不同**的信息合成监督墙需要的形状：
 *
 * ```text
 * GET /monitor（业务状态，每 10 秒刷新）        LiveKit（媒体事实，事件驱动）
 *        ↓                                              ↓
 *   students: MonitorStudent[]              mediaStates / subscriptions
 *        └──────────────────┬───────────────────────────┘
 *                     Monitor Card（视图）
 * ```
 *
 * 七条纪律：
 *
 * 1. **业务状态只用 DTO**（§51）。`students` 只会被 monitor 响应整体替换，
 *    绝不因为"房间里还有这个 participant"就把某个学生标成在线。
 * 2. **订阅是一个"计划"，不是一串动作**（§52）。可见性、Focus、页面隐藏三路输入
 *    先合成"现在该订谁、订多清楚"，再由 `applySubscriptionPlan()` 一次性收敛。
 *    三路各自去调订阅，就会出现"滚动时订阅/退订互相打架"这类只在真机上偶发的 bug。
 * 3. **凭据只存内存**（§44）：`credentials` 是闭包变量，不进响应式 state。
 * 4. **断线是可恢复的失败**：媒体连接断开 → `mediaError` + 重试入口（重新申请
 *    media-token）；而不是把整页变成空白。
 * 5. **轮询间隔 10 秒**（任务书 §Phase 6/7），理由与 §49 学生侧相同：老师关不关课堂、
 *    学生断没断是低频事件。Phase 8 接入 WebSocket 后这个定时器整体删除（§47）。
 * 6. **一次只跑一轮协调**：三路输入都会触发协调，交错执行会让"该订的集合"在两次
 *    await 之间变化（重复退订、把刚订好的又退掉）。串行化 + 合并重跑是最省心的写法。
 * 7. **Focus 放在 store 而不是视图**：它不是纯视图开关，而是直接决定订阅画质
 *    （§30 的"Focus 优先较高画质"）。放在视图里，画质决策就会分裂成两处。
 */
export const useMonitorStore = defineStore('teacher-monitor', () => {
  /** 轮询间隔：见文件头第 5 点。 */
  const MONITOR_POLL_MS = 10_000

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

  /** 老师正在 Focus 看的学生（§30）；null = 在网格视图。 */
  const focusedStudentId = ref<string | null>(null)

  /** 页面是否处于不可见状态（老师切到了别的标签页）。 */
  const pageHidden = ref(false)

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
        if (currentClassroomId !== null) stopPolling()
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
    await reconcileSubscriptions()
  }

  /** 只清本地记录（订阅对象、画质、媒体状态）；不碰连接。 */
  function dropLocalSubscription(identity: string): void {
    pending.delete(identity)
    subscriptions.value = omitKey(subscriptions.value, identity)
    subscriptionQualities.value = omitKey(subscriptionQualities.value, identity)
    mediaStates.value = { ...mediaStates.value, [identity]: 'none' }
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
  }

  function startPolling(): void {
    if (pollTimer !== null || currentClassroomId === null) return
    /**
     * 页面隐藏时**继续**轮询：一次 GET /monitor 的成本可以忽略，而它让老师切回来时
     * 看到的人数、徽章都是新的（订阅是另一回事，见 desiredSubscriptions）。
     */
    pollTimer = setInterval(() => {
      void refresh()
    }, MONITOR_POLL_MS)
  }

  /** 拉一次 monitor 数据并重新协调订阅（轮询与"重试"按钮共用）。 */
  async function refresh(): Promise<void> {
    const classroomId = currentClassroomId
    if (classroomId === null) return
    const seq = ++loadSeq
    loading.value = true
    try {
      const next = await getMonitor(classroomId)
      if (seq !== loadSeq) return
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

  function isFocusedStudent(student: MonitorStudent): boolean {
    return student.studentId === focusedStudentId.value
  }

  /**
   * 进入监督墙：申请媒体凭据 → 连接 → 拉 monitor → 订阅 → 开始轮询。
   *
   * 两步失败的处理不同：媒体失败仍然要把 monitor 数据拉出来（老师至少能看到
   * "谁在上课"），业务失败也不影响已经建立的媒体连接。这两件事各自可重试。
   */
  async function load(classroomId: string): Promise<void> {
    currentClassroomId = classroomId
    await ensureMediaConnected(classroomId)
    await refresh()
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
    pending.clear()
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
    await teardownMedia()
    currentClassroomId = null
    students.value = []
    loaded.value = false
    error.value = null
    mediaError.value = null
    loading.value = false
    focusedStudentId.value = null
    pageHidden.value = false
    visibleStudentIds.clear()
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
    subscribedCount,
    rosterCount,
    enteredCount,
    focusedStudentId,
    focusedStudent,
    pageHidden,
    badgeOf,
    tileStateOf,
    mediaStateOf,
    subscriptionOf,
    qualityOf,
    setStudentVisible,
    setFocusedStudent,
    setPageHidden,
    load,
    refresh,
    retryMedia,
    reconcileSubscriptions,
    stop,
  }
})
