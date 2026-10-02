import type { ConnectionQualityLevel } from './media/media-room.ts'

/**
 * 连接质量 → 学生界面上的中文 + 颜色（§56「网络 ● Good」/ §52）。
 *
 * WHY 不直接把 SDK 的英文枚举显示出来：§56 明确要求这一行是给学生看的，
 * 而 `Poor` / `Lost` 这种词在中文语境里容易被读成同一件事——实际上它们的
 * 下一步动作完全不同（前者"画面可能变糊"，后者"已经断了，需要重进课堂"）。
 *
 * tone 用的是 @classwatch/ui 的 StatusTone 词表（open / closed / danger / warning /
 * neutral），与班级状态、账号状态共用一套语义色，避免界面出现第四种"绿"。
 */
export type ConnectionQualityTone = 'open' | 'warning' | 'danger' | 'neutral'

export interface ConnectionQualityDisplay {
  label: string
  tone: ConnectionQualityTone
  /** 给学生的一句话解释；`null` 表示这一档不需要额外说明。 */
  hint: string | null
}

/**
 * 五档的完整映射。
 *
 * `excellent` 与 `good` 刻意用**不同**的中文（极佳 / 良好）而不是都写"好"：
 * §56 的示例是 "● Good"，学生看到"良好"时若偶尔变成"极佳"会知道网络变好了；
 * 两档都写"好"就丢掉了这个信息。
 */
const QUALITY_DISPLAY: Record<ConnectionQualityLevel, ConnectionQualityDisplay> = {
  excellent: { label: '极佳', tone: 'open', hint: null },
  good: { label: '良好', tone: 'open', hint: null },
  poor: { label: '较差', tone: 'warning', hint: '画面可能变糊或卡顿。' },
  lost: { label: '已断开', tone: 'danger', hint: '与课堂的连接中断了，请重新进入课堂。' },
  unknown: { label: '未知', tone: 'neutral', hint: '还没有收到网络质量数据。' },
}

export function describeConnectionQuality(
  quality: ConnectionQualityLevel,
): ConnectionQualityDisplay {
  return QUALITY_DISPLAY[quality]
}
