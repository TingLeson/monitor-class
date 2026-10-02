import type { IsoDateTime } from '@classwatch/shared-types'

/**
 * 时间展示（§8 currentRun.openedAt）。
 *
 * WHY 保留字符串形态、只在展示层格式化（见 shared-types 的 IsoDateTime 说明）：
 * 后端发的是 RFC3339 UTC，前端不做时区推断，交给浏览器按本地时区渲染。
 * 解析失败返回破折号而不是 "Invalid Date"——后者会让人以为数据坏了。
 */
export function formatDateTime(value: IsoDateTime | null | undefined): string {
  if (!value) return ''
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return '—'
  return parsed.toLocaleString('zh-CN', { hour12: false })
}

/**
 * 只取时分（列表里的"本次开始于 14:05"）。
 *
 * 为什么不上完整日期：老师问的是"这节课开了多久了"，同一节课必然发生在今天，
 * 多出来的年月日只会把卡片压满、把状态信息淹掉（§35 低噪声）。
 */
export function formatTimeOfDay(value: IsoDateTime | null | undefined): string {
  if (!value) return ''
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return '—'
  return parsed.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
}
