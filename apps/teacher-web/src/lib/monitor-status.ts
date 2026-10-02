import type { MonitorStudent } from '@classwatch/shared-types'

/**
 * 监督卡片的状态映射（§22 / §29 / §51）。
 *
 * 这里是**业务状态 + 媒体状态合成卡片**那一步的一半（另一半在 store 里）：
 *
 * ```text
 * Monitor DTO（业务：谁在上课、屏幕是否中断）   ← 本文件
 *        +
 * LiveKit 订阅状态（媒体：画面是否真的到了）    ← store / 组件
 *        ↓
 * Monitor Card
 * ```
 *
 * 关键纪律（§51）：状态徽章**只看 DTO**，不看 LiveKit。老师界面里"这个学生在不在上课"
 * 是一个业务事实，只有后端（Session + Event + webhook）能回答；媒体轨道只是
 * "画面有没有到"。一旦用 participant 是否存在来推导徽章，一次 webhook 延迟就会
 * 让老师看到"学生还在，其实早就断了"。
 */

/**
 * 徽章语义色。
 *
 * 用 @classwatch/ui 的 **BadgeTone** 词表（不是 StatusDot 的 StatusTone）：
 * 监督墙上的徽章是一个分类标签而不是"运行状态"，借用 open/closed 那套词
 * 会让"未连接"看起来像"课堂已关闭"（§35 的语义色纪律）。
 */
export type MonitorBadgeTone = 'positive' | 'danger' | 'muted'

export interface MonitorBadge {
  /** §29 的三种徽章：🟢 正常 / 🔴 屏幕中断 / ⚪ 未连接。 */
  emoji: string
  label: string
  tone: MonitorBadgeTone
}

/**
 * 徽章映射。
 *
 * 判定顺序（顺序即优先级）：
 * 1. `SCREEN_LOST` → 🔴 屏幕中断。这是 §22 的专用状态，后端已经明确告诉我们
 *    "这个学生屏幕断了"，不必再去看 `screen.active`；
 * 2. `ONLINE` 但 `screen.active === false` → 同样按屏幕中断处理。§21 的不变量是
 *    "ONLINE ⇒ screen track 存在且在发布"，这个组合说明不变量已经被破坏，
 *    显示"正常"等于对老师撒谎，而这是监督系统里最不能犯的错；
 * 3. 其余（CONNECTING / DISCONNECTED / LEFT / ROOM_CLOSED）→ ⚪ 未连接。
 *    注意**不能**把这几档归到"屏幕中断"：屏幕中断意味着"学生还在，只是没画面"，
 *    而这几档是"学生根本不在课堂上"，老师要做的事完全不同（一个去提醒学生，
 *    一个去确认学生是不是掉线了）。
 */
export function describeMonitorBadge(student: MonitorStudent): MonitorBadge {
  if (student.sessionStatus === 'SCREEN_LOST') {
    return { emoji: '🔴', label: '屏幕中断', tone: 'danger' }
  }
  if (student.sessionStatus === 'ONLINE') {
    return student.screen.active
      ? { emoji: '🟢', label: '正常', tone: 'positive' }
      : { emoji: '🔴', label: '屏幕中断', tone: 'danger' }
  }
  return { emoji: '⚪', label: '未连接', tone: 'muted' }
}

/**
 * 这张卡片是否应该订阅屏幕轨道（§52 的"按需订阅"在 Phase 6 的最小形态）。
 *
 * 判据只有两个：业务上屏幕是活的（`screen.active`），且有会话可以对应到
 * LiveKit identity。**刻意不**用"当前可见/滚动位置"来筛——那属于 Phase 7 的
 * 动态订阅优化（§52 / §73），本 Phase 先把"不多订阅、不重复订阅"做对。
 */
export function shouldSubscribeScreen(student: MonitorStudent): boolean {
  return student.screen.active && student.sessionId !== null && student.sessionId !== ''
}

/**
 * 卡片主体该显示什么。
 *
 * 四种情形必须区分开，因为老师看到的"没有画面"有四种完全不同的原因：
 * - `waiting`：业务上说他该在共享，但媒体还没到 → "正在订阅画面…"
 * - `lost`：业务上他就没在共享 → "等待共享"
 * - `offline`：他不在课堂上 → "未连接"
 * - `failed`：媒体订阅失败 → 给重试入口（视图负责）
 */
export type MonitorTileBody = 'waiting' | 'lost' | 'offline' | 'failed' | 'live'

/** 单个学生的媒体订阅状态（由 store 维护，界面据此渲染主体）。 */
export type MonitorMediaState = 'none' | 'pending' | 'subscribed' | 'failed'

export function describeTileBody(
  student: MonitorStudent,
  media: { state: MonitorMediaState; hasSubscription: boolean },
): MonitorTileBody {
  if (media.state === 'failed') return 'failed'
  if (media.hasSubscription) return 'live'
  const badge = describeMonitorBadge(student)
  if (badge.label === '未连接') return 'offline'
  if (!student.screen.active) return 'lost'
  return 'waiting'
}

/** 卡片主体文案（与上面的四种情形一一对应）。 */
export const TILE_BODY_TEXT: Record<Exclude<MonitorTileBody, 'live'>, string> = {
  waiting: '正在订阅画面…',
  lost: '等待共享',
  offline: '未连接',
  failed: '画面订阅失败',
}
