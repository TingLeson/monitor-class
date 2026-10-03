import type { RealtimeConnectionState, RealtimeSocket } from '@classwatch/api-client'
import type { RealtimeEvent } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { createRealtimeChannel } from '../lib/realtime-channel.ts'
import { useClassroomsStore } from './classrooms.ts'
import { useMonitorStore } from './monitor.ts'

/**
 * 老师端实时通道（§47 / §74）—— 传输 + 路由。
 *
 * ```text
 * /ws/teacher ──解析──→ RealtimeEvent ──路由──→ monitor store（监督墙）
 *                                          └→ classrooms store（课堂列表 / 详情）
 * ```
 *
 * 三条纪律（与学生端同一套）：
 *
 * 1. **业务判断不在这里**：本文件只回答"这条事件该给谁"。
 * 2. **连接跟随登录态**（App.vue watch `session.isAuthenticated`）：监督墙是老师
 *    唯一的现场，登出后还留着连接等于留着一条已经不被授权的通道。**一个入口只有
 *    一条连接**：监督墙、课堂列表、课堂详情共用它，因此老师在页面之间跳转时
 *    不会出现"事件收不到"的空窗。
 * 3. **HMR 必须关连接**：开发时改这个文件会重新执行模块，不关旧连接就会每保存一次
 *    多一条——表现为"一个事件被处理了 N 遍"，排查成本极高。
 */

/** 模块级引用：HMR dispose 只能捕获模块作用域（见上第 3 点）。 */
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

export const useRealtimeStore = defineStore('teacher-realtime', () => {
  const state = ref<RealtimeConnectionState>('closed')
  /** 是否已经被启动过（区分"还没连"与"连过但断了"，见学生端同名说明）。 */
  const started = ref(false)
  /**
   * 是否**曾经**连上过。
   *
   * 只影响一句话：首次连接失败叫"正在连接"，连上过又断了才叫"正在重连"。
   * 这不是连接状态（冻结词表只有三态），而是文案需要的上下文。
   */
  const wasConnected = ref(false)
  /** 是否已判定"握手被拒"（最可能是会话失效；启发式，见 api-client）。 */
  const authFailed = ref(false)
  const droppedMessages = ref(0)
  const lastEventAt = ref<string | null>(null)

  const isOpen = computed(() => state.value === 'open')

  function start(): void {
    if (channel !== null) return
    started.value = true
    channel = createRealtimeChannel({
      onEvent: handleEvent,
      onStateChange: handleStateChange,
    })
    handleStateChange(channel.state)
    channel.connect()
  }

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
    useMonitorStore().setRealtimeState('closed')
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
   * 连接状态变化：写进 state，并转给监督 store。
   *
   * 监督 store 需要它做两件事：决定兜底快照的频率（通道断了就收紧），
   * 以及**重连成功后立刻补一次快照**——断线期间的事件是永久丢失的，
   * 只靠后续事件永远补不回来（§51 的 DTO 快照正是为这种时刻准备的）。
   */
  function handleStateChange(next: RealtimeConnectionState): void {
    const wasOpen = state.value === 'open'
    state.value = next
    if (next === 'open') wasConnected.value = true
    authFailed.value = channel?.authFailed ?? false
    droppedMessages.value = channel?.droppedMessages ?? 0
    useMonitorStore().setRealtimeState(next)
    if (!wasOpen && next === 'open') {
      void useMonitorStore().refresh()
      void useClassroomsStore().resyncAfterRealtimeReconnect()
    }
  }

  /**
   * 事件路由（§47 的收件人表）。
   *
   * 注意 `ROOM_OPENED` **不在**老师端的收件人里（冻结契约：它只发给学生）；
   * 老师自己开课走的是 HTTP，响应里已经带着新的课堂状态。因此这里落在 default
   * 分支——不做"顺手也支持一下"的扩展，那会掩盖契约与实现的分歧。
   *
   * `PRIVATE_TALK_*`（§31）**必须**进监督 store：目标可能是在另一个标签页里
   * 发起的，而后端在沟通结束时也会广播 `PRIVATE_TALK_ENDED`。少了这条路由，
   * 老师的界面就会停在一个与服务端不一致的"我正在对谁讲话"上——
   * 而那句话关系到他的麦克风正被谁听见。
   */
  function handleEvent(event: RealtimeEvent): void {
    lastEventAt.value = event.at
    const monitor = useMonitorStore()
    switch (event.type) {
      case 'ROOM_CLOSED':
        // 两个页面都要跟着变：监督墙（画面与提示）与课堂详情/列表（状态徽章）。
        useClassroomsStore().applyRealtimeEvent(event)
        monitor.applyRealtimeEvent(event)
        break
      case 'STUDENT_ONLINE':
      case 'STUDENT_OFFLINE':
      case 'SCREEN_LOST':
      case 'SCREEN_RESTORED':
      case 'CAMERA_CHANGED':
      case 'MIC_CHANGED':
      case 'PRIVATE_TALK_STARTED':
      case 'PRIVATE_TALK_ENDED':
        monitor.applyRealtimeEvent(event)
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
