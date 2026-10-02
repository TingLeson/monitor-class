import type { IsoDateTime } from '@classwatch/shared-types'

/**
 * 时间展示（§9 lastLoginAt）。
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
