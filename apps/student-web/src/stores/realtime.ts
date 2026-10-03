import type { RealtimeConnectionState, RealtimeSocket } from '@classwatch/api-client'
import type { RealtimeEvent } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { createRealtimeChannel } from '../lib/realtime-channel.ts'
import { useClassroomsStore } from './classrooms.ts'
import { useMediaSessionStore } from './media-session.ts'

/**
 * 学生端实时通道（§47 / §74）—— 传输 + 路由。
 *
 * 它是**唯一**持有 WebSocket 的地方，也只做两件事：
 *
 * ```text
 * /ws/student ──解析──→ RealtimeEvent ──路由──→ classrooms store（列表）
 *                                            └→ media-session store（在课会话）
 * ```
 *
 * 三条纪律：
 *
 * 1. **业务判断不进这里**。哪个事件该改什么状态，由对应的 store 决定；
 *    本文件只回答"这条事件该给谁"，于是"学生端会消费哪些事件"一眼可查。
 * 2. **连接生命周期跟随登录态**（App.vue 里 watch `session.isAuthenticated`）：
 *    未登录就开一条注定 401 的连接是白白打后端；登出后还留着连接更糟——
 *    那是一条已经不被授权的通道。
 * 3. **HMR 必须关连接**：改这个文件时 Vite 会重新执行模块，而 Pinia 的旧实例
 *    可能还活着。不主动 close 的话，开发时每保存一次就多一条连接，
 *    最后表现成"事件被处理了 N 遍"这种极难定位的假 bug。
 */

/**
 * 模块级引用，而不是 store 内部变量。
 *
 * WHY：HMR 重新执行本模块时，只有模块级的作用域能被 `import.meta.hot.dispose`
 * 捕获到；写在 store 的 setup 闭包里就没人能关掉那条旧连接了。
 */
let channel: RealtimeSocket | null = null

if (import.meta.hot) {
  import.meta.hot.dispose(() => {
    channel?.close()
    channel = null
  })
}

/**
 * 丢弃模块级的连接引用（**仅供测试**）。
 *
 * WHY 需要它：连接引用活在模块作用域（HMR 那条理由），因此它比任何一次测试用例
 * 都活得久。测试替身必须在用例之间把它清掉，否则第二个用例的 `start()` 会因为
 * "已经有一条连接"直接返回，整个文件只会有一条假连接。
 */
export function resetRealtimeChannel(): void {
  channel?.close()
  channel = null
}

export const useRealtimeStore = defineStore('student-realtime', () => {
  /** 连接状态（冻结的三态词表）。 */
  const state = ref<RealtimeConnectionState>('closed')
  /**
   * 通道是否已经被本 store 启动过。
   *
   * WHY 需要它：`state === 'closed'` 有两种完全不同的含义——"还没开始连"
   * 与"连过但断了"。界面在这两种情况下要说的话不一样（前者不该出现任何提示），
   * 而冻结词表里没有第四个值可用来表达它，所以在**应用层**补一个布尔。
   */
  const started = ref(false)
  /**
   * 是否**曾经**连上过。
   *
   * 只影响一句话：首次连接失败叫"正在连接"，连上过又断了才叫"正在重连"。
   * 这不是连接状态（冻结词表只有三态），而是文案需要的上下文。
   */
  const wasConnected = ref(false)
  /**
   * 是否已判定"握手被拒"（最可能是会话失效）。
   *
   * 浏览器不暴露握手失败的状态码，所以这是一个**启发式**结论（连续多次连开都没开起来，
   * 见 api-client 的 handshakeFailureLimit）。UI 因此只说"通道未建立、可能需要重新登录"，
   * 不说"你被登出了"。
   */
  const authFailed = ref(false)
  /** 被丢弃的报文数（解析失败 / 未知类型）：排障用，不进界面。 */
  const droppedMessages = ref(0)
  /** 最近一条事件的服务端时间；断线期间界面可以据此说"数据停在什么时候"。 */
  const lastEventAt = ref<string | null>(null)

  const isOpen = computed(() => state.value === 'open')

  /**
   * 开始（幂等）。已登录时才调用；重复调用不会建第二条连接。
   */
  function start(): void {
    if (channel !== null) return
    started.value = true
    channel = createRealtimeChannel({
      onEvent: handleEvent,
      onStateChange: handleStateChange,
    })
    // 先把状态同步给域 store，再发起连接：让"兜底轮询频率"从第一帧起就是对的。
    handleStateChange(channel.state)
    channel.connect()
  }

  /** 停止并丢弃当前通道（登出、根组件卸载）。再次 start() 会得到一条全新的连接。 */
  function stop(): void {
    const current = channel
    channel = null
    current?.close()
    started.value = false
    wasConnected.value = false
    state.value = 'closed'
    authFailed.value = false
    droppedMessages.value = 0
    lastEventAt.value = null
    useMediaSessionStore().setRealtimeState('closed')
  }

  /**
   * 手动重试：丢掉当前通道，重新开一条（UI 只在 authFailed 时提供这个动作）。
   *
   * WHY 需要它：客户端在"连续多次连开都没开起来"之后会停止自动重连（那是 §47
   * "不要无限重连"的要求）。如果原因是服务端短暂不可用，页面不应该只能靠刷新恢复；
   * 由用户点一下按钮重新走一轮退避，既不打扰正处于会话失效状态的用户，
   * 也让"服务端回来了"这件事有一个明确的恢复入口。
   */
  function restart(): void {
    stop()
    start()
  }

  /**
   * 连接状态变化：写进 state，并把"通道可用性"转给在课会话。
   *
   * 在课会话需要它的唯一原因是**兜底轮询的频率**：通道开着时 60 秒一次足够，
   * 通道断了就必须收紧，否则"老师关了课堂"这件事可能被拖到一个很晚的时刻才被发现。
   */
  function handleStateChange(next: RealtimeConnectionState): void {
    const wasOpen = state.value === 'open'
    state.value = next
    if (next === 'open') wasConnected.value = true
    authFailed.value = channel?.authFailed ?? false
    droppedMessages.value = channel?.droppedMessages ?? 0
    useMediaSessionStore().setRealtimeState(next)
    if (!wasOpen && next === 'open') {
      /**
       * 重连成功立刻补一次快照。
       *
       * WHY：断线期间发生的事（尤其是老师关课）只有快照能补上；不补的话，
       * 学生要么等一个兜底周期，要么一直在共享一块老师已经不需要的屏幕。
       * `refreshClassroom` 在没有活会话时是空操作，因此这里不需要额外判断。
       */
      void useMediaSessionStore().refreshClassroom()
    }
  }

  /**
   * 事件路由（§47 的收件人表）。
   *
   * `STUDENT_*` / `CAMERA_*` / `MIC_*` / `PRIVATE_TALK_*` 都不该发给学生：
   * 服务端按 §26 只会推"学生自己被授权的那间课堂 + 自己的会话"。
   * 万一收到，这里**什么也不做**——不是因为懒，而是因为学生端多一条别的学生的
   * 数据，就等于把"谁在线、谁屏幕断了"泄漏给了同学（§26 的学生间隔离）。
   */
  function handleEvent(event: RealtimeEvent): void {
    lastEventAt.value = event.at
    const classrooms = useClassroomsStore()
    const mediaSession = useMediaSessionStore()
    switch (event.type) {
      case 'ROOM_OPENED':
        classrooms.applyRealtimeEvent(event)
        break
      case 'ROOM_CLOSED':
        // 两处都要知道：列表要把"进入课堂"变灰，会话页要断开媒体并停止捕获（§49）。
        classrooms.applyRealtimeEvent(event)
        mediaSession.applyRealtimeEvent(event)
        break
      case 'SCREEN_LOST':
      case 'SCREEN_RESTORED':
        mediaSession.applyRealtimeEvent(event)
        break
      default:
        break
    }
  }

  return {
    state,
    started,
    wasConnected,
    authFailed,
    droppedMessages,
    lastEventAt,
    isOpen,
    start,
    stop,
    restart,
    handleEvent,
  }
})
