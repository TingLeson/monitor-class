import type { StudentClassroom } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  createScreenPublisherRoom,
  type ConnectionQualityLevel,
  type MediaCredentials,
  type ScreenPublisherRoom,
} from '../lib/media/media-room.ts'
import {
  CLASSROOM_STATUS_POLL_MS,
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
 * 4. **业务状态与媒体状态分开**（§51 的同一条原则用在学生端）：老师是否关了课堂
 *    由后端（低频轮询）说了算，媒体连接是否还在由 LiveKit 说了算，两者在
 *    `phase` 这一层合成，谁都不去假装对方。
 */
export const useMediaSessionStore = defineStore('student-media-session', () => {
  /* ---------------------------------------------------------------------- */
  /* 可被界面读取的状态                                                      */
  /* ---------------------------------------------------------------------- */

  const phase = ref<MediaSessionPhase>('idle')
  /** 当前会话 id；join / retry 之后可能变化（§50：后端可以取代旧连接）。 */
  const sessionId = ref<string | null>(null)
  /** 课堂详情（名称 + 状态）。轮询会刷新它，§49 的"老师已关闭"就来自这里。 */
  const classroom = ref<StudentClassroom | null>(null)
  const quality = ref<ConnectionQualityLevel>('unknown')
  const failure = ref<MediaFailure | null>(null)
  /** 客户端是否正在等待连接完成（用于按钮 loading，不参与状态判断）。 */
  const busy = ref(false)
  /** LiveKit 正在自动重连（§52）；界面据此显示"正在重连"，不改 phase。 */
  const reconnecting = ref(false)

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
  /** 房间事件与轨道 ended 的取消订阅函数；退出时必须逐个调用。 */
  let roomUnsubscribers: (() => void)[] = []
  let unsubscribeEnded: (() => void) | null = null
  /** 课堂状态轮询定时器（§49 的临时手段，Phase 8 会被 WebSocket 取代）。 */
  let pollTimer: ReturnType<typeof setInterval> | null = null
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

  /** 停掉课堂状态轮询（幂等）。 */
  function stopClassroomWatch(): void {
    if (pollTimer === null) return
    clearInterval(pollTimer)
    pollTimer = null
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
    leaveReported = false
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
  /* 课堂状态轮询（§49 学生侧，Phase 8 会被 WebSocket 取代）                  */
  /* ---------------------------------------------------------------------- */

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
    void teardownRoom()
    releaseCapture()
    credentials = null
  }

  /**
   * 拉一次课堂详情。
   *
   * 失败**不改变**课堂状态：一次网络抖动不等于"课堂关了"，据此断开学生的共享
   * 才是真的错误。等下一次轮询即可。
   */
  async function refreshClassroom(): Promise<void> {
    const id = classroomId
    if (id === null) return
    if (phase.value === 'left' || phase.value === 'closed') return
    const seq = ++pollSeq
    try {
      const next = await getClassroom(id)
      if (seq !== pollSeq) return
      classroom.value = next
      if (next.status === 'CLOSED') applyClassroomClosed()
    } catch {
      // 见上：轮询失败保持现状。
    }
  }

  /** 开始低频轮询（见 media-session-state.ts 对间隔的说明）。 */
  function startClassroomWatch(): void {
    if (classroomId === null || pollTimer !== null) return
    // 先立刻拉一次：课堂名要马上显示，而且"老师刚关了课堂"不必等满一个周期。
    void refreshClassroom()
    pollTimer = setInterval(() => {
      void refreshClassroom()
    }, CLASSROOM_STATUS_POLL_MS)
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
    credentials = null
    classroomId = null
    failure.value = null
    reconnecting.value = false
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
    leaveReported = false
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
    isOnline,
    isScreenLost,
    isConnecting,
    isClosedByTeacher,
    hasNoSession,
    hasFailure,
    canRetry,
    prepare,
    begin,
    publishCapture,
    retry,
    refreshClassroom,
    startClassroomWatch,
    stopClassroomWatch,
    leave,
    releaseNow,
    reset,
  }
})
