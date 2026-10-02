import type { ClassroomStatus } from '@classwatch/shared-types'
import type { StatusTone } from '@classwatch/ui'

/**
 * Classroom 状态 → 展示映射（§7 / §14）。
 *
 * WHY 单独一个文件：列表卡片与 PreJoin 页必须对同一个状态说**同一句话**。
 * 各写一份的结果是"列表说未开启、详情页说已关闭"，学生会以为是两件不同的事
 * （而 §7 明确只有 OPEN / CLOSED 两种状态，不允许自造第三种措辞）。
 *
 * 措辞固定为「已开启 / 未开启」（§14 的卡片图），不要改成"进行中""已结束"：
 * "进行中"会让人以为课堂里正有事情发生，"已结束"则暗示这节课再也不会开——
 * 而 CLOSED 只是"现在进不去"（老师随时可以再次开启，§48）。
 */

/** 状态文案：OPEN → 已开启，CLOSED → 未开启（§14）。 */
export function classroomStatusLabel(status: ClassroomStatus): string {
  return status === 'OPEN' ? '已开启' : '未开启'
}

/**
 * 状态点语义色（§35 清晰状态颜色）。
 *
 * 用 StatusDot 的 open / closed 而不是 AppBadge：这里表达的是"运行状态"，
 * 与 UI 包里 StatusDot 的语义定义一致（open → Classroom OPEN）。绿色只留给真正
 * 可以进入的课堂——灰色代表"还进不去"，不代表出错，所以绝不能借用 danger。
 */
export function classroomStatusTone(
  status: ClassroomStatus,
): Extract<StatusTone, 'open' | 'closed'> {
  return status === 'OPEN' ? 'open' : 'closed'
}

/**
 * 现在能不能进入这个课堂。
 *
 * WHY 由状态单独决定，而不是看 `currentRun` 是否为 null：契约规定 CLOSED 时
 * currentRun 必然是 null，反向却不成立（status 才是唯一判据，§7）。
 * 界面依赖一个"顺带成立"的巧合，早晚会在后端调整字段时静默失效。
 */
export function canEnterClassroom(status: ClassroomStatus): boolean {
  return status === 'OPEN'
}
