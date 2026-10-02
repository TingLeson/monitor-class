import type { IsoDateTime } from '@classwatch/shared-types'

/**
 * 时间展示（§14「本次开始于」用的就是 currentRun.openedAt）。
 *
 * WHY 只在展示层格式化，不在 store 里把 RFC3339 转成 Date：后端发的是 UTC，
 * 前端不做时区推断，交给浏览器按学生本机的时区渲染——学生看到的必须是
 * "他那块表上的时间"，而不是服务器的。
 *
 * 解析失败返回破折号而不是 "Invalid Date"：后者会让人以为数据坏了。
 */
export function formatTimeOfDay(value: IsoDateTime | null | undefined): string {
  if (!value) return ''
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return '—'
  /*
   * 只取时分（"本次开始于 14:05"）。卡片上要回答的是"这节课开了多久了"，
   * 同一节课必然发生在今天，多出来的年月日只会把卡片压满、把状态淹掉（§35 低噪声）。
   */
  return parsed.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
}
