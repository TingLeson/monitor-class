import { ApiError, isApiError } from '@classwatch/api-client'
import type { MonitorStudent } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, markRaw, ref, shallowRef } from 'vue'
import {
  createMonitorRoom,
  type MediaCredentials,
  type MonitorRoom,
  type ScreenSubscription,
} from '../lib/media/media-room.ts'
import {
  describeMonitorBadge,
  shouldSubscribeScreen,
  type MonitorBadge,
  type MonitorMediaState,
} from '../lib/monitor-status.ts'
import { getMonitor, requestMediaToken } from '../lib/teacher-monitor-api.ts'

/**
 * 老师端监督 store（§27 / §29 / §51 / §52）。
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
 * 五条纪律：
 *
 * 1. **业务状态只用 DTO**（§51）。`students` 只会被 monitor 响应整体替换，
 *    绝不因为"房间里还有这个 participant"就把某个学生标成在线。
 * 2. **订阅只发生在该订阅的学生身上**，且**幂等**（§52）。每 10 秒刷新一次 monitor，
 *    如果每次都对所有 `screen.active` 的学生调一次订阅，就会变成每 10 秒一轮
 *    订阅风暴——`pending`/`subscriptions` 两层守卫就是为这件事存在的。
 * 3. **凭据只存内存**（§44）：`credentials` 是闭包变量，不进响应式 state。
 * 4. **断线是可恢复的失败**：媒体连接断开 → `mediaError` + 重试入口（重新申请
 *    media-token）；而不是把整页变成空白。
 * 5. **轮询间隔 10 秒**（任务书 §Phase 6），理由与 §49 学生侧相同：老师关不关课堂、
 *    学生断没断是低频事件。Phase 8 接入 WebSocket 后这个定时器整体删除（§47）。
 */
export const useMonitorStore = defineStore('teacher-monitor', () => {
  /** 轮询间隔：见文件头第 5 点。 */
  const MONITOR_POLL_MS = 10_000

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

  /* ---------------------------------------------------------------------- */
  /* 只在内存里的私有内容                                                    */
  /* ---------------------------------------------------------------------- */

  /** 媒体凭据（§44）：闭包变量，不进 state、不写 URL、不进日志。 */
  let credentials: MediaCredentials | null = null
  let room: MonitorRoom | null = null
  let roomUnsubscribers: (() => void)[] = []
  /** 正在订阅中的 sessionId：第二次调用直接返回，不产生第二次 setSubscribed。 */
  const pending = new Set<string>()
  let pollTimer: ReturnType<typeof setInterval> | null = null
  let currentClassroomId: string | null = null
  /** 请求序号：晚发出的响应才能写状态。 */
  let loadSeq = 0

  const isEmpty = computed(() => loaded.value && students.value.length === 0)
  /** 需要订阅屏幕的学生数（用于页头"正在监督 N 路画面"）。 */
  const subscribedCount = computed(() => Object.keys(subscriptions.value).length)

  /** 每个学生的徽章（视图直接读，避免在模板里散落判断逻辑）。 */
  function badgeOf(student: MonitorStudent): MonitorBadge {
    return describeMonitorBadge(student)
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
        pending.delete(identity)
        subscriptions.value = omit(subscriptions.value, identity)
        mediaStates.value = { ...mediaStates.value, [identity]: 'none' }
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
      pending.delete(identity)
      subscriptions.value = omit(subscriptions.value, identity)
      mediaStates.value = { ...mediaStates.value, [identity]: 'none' }
      // 适配层里对已经离开的 participant 是空操作；这里调用是为了对称与幂等。
      await active.unsubscribeScreen(identity)
    }
    await reconcileSubscriptions()
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
  /* 订阅协调（§52 的手动订阅）                                              */
  /* ---------------------------------------------------------------------- */

  /**
   * 让"当前该订阅的学生"与"已建立的订阅"对齐。
   *
   * - `screen.active` 且未订阅 → 订阅（pending 守卫防重复）；
   * - 不再 `screen.active`（或学生不在列表里了）→ 取消订阅，停止下行。
   *
   * 每次 monitor 刷新都会调用，因此**必须**便宜且幂等：绝大多数情况下它什么也不做。
   */
  async function reconcileSubscriptions(): Promise<void> {
    const active = room
    if (active === null) return

    const wanted = new Set<string>()
    for (const student of students.value) {
      if (shouldSubscribeScreen(student) && student.sessionId !== null)
        wanted.add(student.sessionId)
    }

    // 先取消不再需要的订阅（学生停止共享、离开、或状态回到未连接）。
    for (const identity of Object.keys(subscriptions.value)) {
      if (wanted.has(identity)) continue
      subscriptions.value = omit(subscriptions.value, identity)
      mediaStates.value = { ...mediaStates.value, [identity]: 'none' }
      await active.unsubscribeScreen(identity)
    }

    for (const identity of wanted) {
      if (subscriptions.value[identity] !== undefined || pending.has(identity)) continue
      pending.add(identity)
      mediaStates.value = { ...mediaStates.value, [identity]: 'pending' }
      try {
        const subscription = await active.subscribeScreen(identity)
        if (subscription === null) {
          // 业务说他在共享，但媒体里还没有这条轨道：可能是刚发布、也可能后端状态超前。
          // 保持 'none'，下一次轮询/事件会再试一次。
          mediaStates.value = { ...mediaStates.value, [identity]: 'none' }
          continue
        }
        subscriptions.value = { ...subscriptions.value, [identity]: markRaw(subscription) }
        mediaStates.value = { ...mediaStates.value, [identity]: 'subscribed' }
      } catch {
        mediaStates.value = { ...mediaStates.value, [identity]: 'failed' }
      } finally {
        pending.delete(identity)
      }
    }
  }

  /** 不可变删除（避免直接改 shallowRef 里的对象）。 */
  function omit(
    source: Record<string, ScreenSubscription>,
    key: string,
  ): Record<string, ScreenSubscription> {
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
      await reconcileSubscriptions()
    } catch (cause) {
      if (seq !== loadSeq) return
      /**
       * 业务面失败**不清空**已有数据：把刚看到的画面抹掉，老师会以为学生都掉线了。
       * 保留上一轮数据 + 一句错误提示，比一片空白诚实得多。
       */
      error.value = isApiError(cause) ? cause : new ApiError({ code: 'INTERNAL', cause })
    } finally {
      if (seq === loadSeq) loading.value = false
    }
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

  /** 离开页面：停止轮询、断开媒体、清空一切。 */
  async function stop(): Promise<void> {
    stopPolling()
    await teardownMedia()
    currentClassroomId = null
    students.value = []
    loaded.value = false
    error.value = null
    mediaError.value = null
    loading.value = false
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
    subscribedCount,
    badgeOf,
    mediaStateOf,
    subscriptionOf,
    load,
    refresh,
    retryMedia,
    reconcileSubscriptions,
    stop,
  }
})
