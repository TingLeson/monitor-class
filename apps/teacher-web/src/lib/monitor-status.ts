import type { ConnectionQuality, MonitorStudent, MonitorTileState } from '@classwatch/shared-types'
import type { RealtimeConnectionState } from '@classwatch/api-client'

/**
 * 监督快照的**兜底**刷新间隔（§47 / §51）。
 *
 * Phase 8 起监督墙的主路径是 `/ws/teacher` 的实时事件；`GET monitor` 退回三个
 * 不可替代的位置：首屏快照、**名单的权威**（学生被加入课堂不发事件）、以及漏事件时的兜底。
 *
 * 两档的理由：
 * - 实时通道正常（60 秒）：事件已经覆盖了"谁上线/下线/屏幕断了"，快照只是防漏。
 *   频率必须低，否则等于把事件驱动又做回了轮询；
 * - 实时通道断开（20 秒）：这段时间里监督墙**只有**快照，老师的判断完全建立在它上面。
 *   20 秒是"看不出明显延迟"与"不打爆后端"之间的折中（一个班几十人，老师端只有一两个）。
 */
export const MONITOR_FALLBACK_POLL_MS = 60_000
export const MONITOR_DEGRADED_POLL_MS = 20_000

/** 按实时通道状态挑一个兜底间隔（`open` 以外的一切都按降级处理）。 */
export function monitorFallbackPollMs(realtime: RealtimeConnectionState): number {
  return realtime === 'open' ? MONITOR_FALLBACK_POLL_MS : MONITOR_DEGRADED_POLL_MS
}

/**
 * 监督卡片的状态映射（§12 / §22 / §29 / §30 / §51）。
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
 * 会让"未进入"看起来像"课堂已关闭"（§35 的语义色纪律）。
 *
 * 四档而不是三档：Phase 7 的名单里有"连接中"（🟡），它既不是正常也不是故障，
 * 借用 danger 会让老师以为出了问题，借用 muted 又会让它淹没在一片灰里。
 */
export type MonitorBadgeTone = 'positive' | 'danger' | 'muted' | 'attention'

export interface MonitorBadge {
  /** §29 的状态色，Phase 7 覆盖 6 个落库状态 + 未进入。 */
  emoji: string
  label: string
  tone: MonitorBadgeTone
}

/**
 * 会话是否"在线类"：学生本人确实在这节课里。
 *
 * 只包含 ONLINE / SCREEN_LOST：
 * - CONNECTING 是"后端已建 Session、学生还没连上媒体"（§12），人还没进来；
 * - DISCONNECTED 是"连过但断了"，需要老师去确认，不是"在上课"；
 * - LEFT / ROOM_CLOSED 是已经结束。
 *
 * WHY 把它单独提出来：头部计数（已进入 N）与订阅判据必须用**同一把尺子**，
 * 否则会出现"头说 5 人在线，网格里只有 3 张卡片有画面"这种自己和自己矛盾的界面。
 */
function isSessionLive(status: MonitorStudent['sessionStatus']): boolean {
  return status === 'ONLINE' || status === 'SCREEN_LOST'
}

/**
 * 学生是否已经进入本次 Run（头部"已进入 N"的分子，也是"能不能有画面"的前提）。
 *
 * 两个条件缺一不可：有会话 identity（`sessionId`），且会话在线。
 * 后端契约保证二者同时为空或同时有值（见 shared-types 的 MonitorStudent 说明），
 * 这里仍然两个都判，是因为**媒体订阅必须拿到 identity**，而"没有 identity 却显示在线"
 * 的一行如果在运行期真的出现，宁可算作未进入，也不能拿一个空字符串去订轨道。
 */
export function isStudentEntered(student: MonitorStudent): boolean {
  return (
    student.sessionId !== null && student.sessionId !== '' && isSessionLive(student.sessionStatus)
  )
}

/**
 * 派生展示状态（§29）。
 *
 * 判定顺序即优先级，两条容易写错的边界：
 * 1. **`sessionStatus === null` 优先于一切**：没有会话就没有 screen 状态可言，
 *    必须显示"未进入"。这是老师最需要一眼看到的信息——名单里 25 个人到底进来了几个。
 * 2. `ONLINE` 但 `screen.active === false` 归到 `SCREEN_LOST`。§21 的不变量是
 *    "ONLINE ⇒ screen track 存在且在发布"，这个组合说明不变量已经被破坏；
 *    显示"正常"等于对老师撒谎，而这是监督系统里最不能犯的错。
 */
export function deriveMonitorTileState(student: MonitorStudent): MonitorTileState {
  switch (student.sessionStatus) {
    case null:
      return 'NOT_JOINED'
    case 'CONNECTING':
      return 'CONNECTING'
    case 'ONLINE':
      return student.screen.active ? 'NORMAL' : 'SCREEN_LOST'
    case 'SCREEN_LOST':
      return 'SCREEN_LOST'
    case 'DISCONNECTED':
      return 'DISCONNECTED'
    case 'LEFT':
    case 'ROOM_CLOSED':
      // 合并成一档：对老师来说"学生自己走了"和"课堂关掉了"是同一件事——
      // 这个人已经不在这节课里，能采取的动作没有差别（§29 的六色语义）。
      return 'LEFT'
  }
}

/**
 * 徽章映射（§29 的六色 + 未进入）。
 *
 * WHY 断开（⚪）和已离开（⚫）要用两个颜色：它们都"没有画面"，但老师要做的事
 * 完全不同——断开的要判断是不是网络问题、要不要等重连；离开的是这件事已经结束。
 * 把两者涂成同一个灰，监督墙就退化成了"一片灰"。
 */
export function describeMonitorBadge(student: MonitorStudent): MonitorBadge {
  switch (deriveMonitorTileState(student)) {
    case 'NORMAL':
      return { emoji: '🟢', label: '正常', tone: 'positive' }
    case 'SCREEN_LOST':
      return { emoji: '🔴', label: '屏幕中断', tone: 'danger' }
    case 'CONNECTING':
      return { emoji: '🟡', label: '连接中', tone: 'attention' }
    case 'DISCONNECTED':
      return { emoji: '⚪', label: '已断开', tone: 'muted' }
    case 'LEFT':
      return { emoji: '⚫', label: '已离开', tone: 'muted' }
    case 'NOT_JOINED':
      return { emoji: '⚪', label: '未进入', tone: 'muted' }
  }
}

/**
 * 这张卡片是否**值得**订阅屏幕轨道（§52 的业务侧判据，可见性由 store 再叠一层）。
 *
 * 三个条件：会话在线 + 屏幕在发布 + 有 identity 可用。
 *
 * WHY 必须包含"会话在线"这一条：名单里那些从未进入、已断开、已离开的学生
 * 不该留下任何订阅（§52 的"离线卡片不保留订阅"）。只判 `screen.active` 会留一个
 * 窗口期——后端把某个学生标成 DISCONNECTED 之后，`screen.active` 可能还会是 true
 * 直到下一次观测，那几秒里老师端会一直挂着一条永远不会有画面的下行。
 */
export function shouldSubscribeScreen(student: MonitorStudent): boolean {
  if (!isStudentEntered(student)) return false
  return student.screen.active
}

/**
 * 卡片主体该显示什么。
 *
 * 七种情形必须区分开，因为老师看到的"没有画面"有七种完全不同的原因：
 * - `waiting`：业务上说他该在共享，媒体还没到 → "正在订阅画面…"
 * - `lost`：屏幕中断 → "等待共享"
 * - `connecting`：还没连上媒体 → "正在连接…"
 * - `disconnected`：连过但掉了 → "连接已断开"
 * - `left`：已经离开 → "已离开课堂"
 * - `notJoined`：本次 Run 从未进入 → "未进入课堂"
 * - `failed`：媒体订阅失败 → "画面订阅失败"（视图给重试入口）
 *
 * 合成规则只有一条：**有订阅就是 live**；没有订阅时，原因完全由业务状态决定
 * （`failed` 例外，那是媒体层的失败，与业务状态无关）。
 */
export type MonitorTileBody =
  'live' | 'waiting' | 'lost' | 'connecting' | 'disconnected' | 'left' | 'notJoined' | 'failed'

/** 单个学生的媒体订阅状态（由 store 维护，界面据此渲染主体）。 */
export type MonitorMediaState = 'none' | 'pending' | 'subscribed' | 'failed'

export function describeTileBody(
  student: MonitorStudent,
  media: { state: MonitorMediaState; hasSubscription: boolean },
): MonitorTileBody {
  if (media.state === 'failed') return 'failed'
  if (media.hasSubscription) return 'live'
  switch (deriveMonitorTileState(student)) {
    case 'NORMAL':
      // 业务说该有画面、媒体还没到。可能是刚发布，也可能是这一格根本不在视口里
      // （§52 的按可见性订阅：不可见的卡片本来就不该有画面）。
      return 'waiting'
    case 'SCREEN_LOST':
      return 'lost'
    case 'CONNECTING':
      return 'connecting'
    case 'DISCONNECTED':
      return 'disconnected'
    case 'LEFT':
      return 'left'
    case 'NOT_JOINED':
      return 'notJoined'
  }
}

/** 卡片主体文案（与上面的七种情形一一对应，`live` 没有文案因为它渲染的是画面）。 */
export const TILE_BODY_TEXT: Record<Exclude<MonitorTileBody, 'live'>, string> = {
  waiting: '正在订阅画面…',
  lost: '等待共享',
  connecting: '正在连接…',
  disconnected: '连接已断开',
  left: '已离开课堂',
  notJoined: '未进入课堂',
  failed: '画面订阅失败',
}

/**
 * 卡片主体占位的补充说明（"为什么没有画面"的下一句）。
 *
 * WHY 要有第二行：只有"未进入课堂"四个字时，老师会怀疑是不是画面没加载出来；
 * 补一句原因，监督墙才能替代"挨个问学生"。
 */
export const TILE_BODY_HINT: Partial<Record<Exclude<MonitorTileBody, 'live'>, string>> = {
  notJoined: '本次课堂该学生尚未进入',
  disconnected: '连接已中断，可能正在重连',
  left: '该学生已结束本次课堂',
  lost: '学生已停止共享，等待重新共享',
}

/**
 * 网格卡片页脚的连接提示（§29 的 `connection`）。
 *
 * 用短词而不是百分比/延迟数字：`connection` 是后端的定性判断（§51），
 * 把它渲染成"32ms"这种精确数字会暗示一个并不存在的数据源。
 * UNKNOWN 单独成词而不是"正常"——老师看到的"未知"是真的没有数据（§51 明令
 * 不许用 GOOD 兜底）。
 */
export function describeConnectionHint(connection: ConnectionQuality): string {
  switch (connection) {
    case 'GOOD':
      return '正常'
    case 'FAIR':
      return '一般'
    case 'POOR':
      return '较差'
    case 'UNKNOWN':
      return '未知'
  }
}

/**
 * Focus 面板的 Network 行（§30 的"Network Good"）。
 *
 * 沿用 §30 线框图的英文等级词：这三档就是 DTO 的取值本身，翻译成中文反而
 * 会让老师无法把它和排障时看到的接口报文对上。UNKNOWN 是"还没有数据"，
 * 不是一个等级，所以它用中文的"未知"——措辞不同正是为了让这件事一眼可辨。
 */
export function describeNetworkLevel(connection: ConnectionQuality): string {
  switch (connection) {
    case 'GOOD':
      return 'Good'
    case 'FAIR':
      return 'Fair'
    case 'POOR':
      return 'Poor'
    case 'UNKNOWN':
      return '未知'
  }
}
