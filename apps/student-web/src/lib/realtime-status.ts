import type { RealtimeConnectionState } from '@classwatch/api-client'

/**
 * 实时通道状态在**学生端**的文案（§47 / §74）。
 *
 * WHY 不放进 shared-types / api-client：那是契约层，而这里说的是"这句话该怎么说"
 * ——同一份状态在老师端的措辞并不一样（老师要的是"状态可能不是最新"，
 * 学生要的是"课堂是否还开着"），硬凑成一份共享文案只会让两边都说得不准确。
 *
 * 三条约束（与 `media-session-state.ts` 的失败文案相同）：
 * 1. 不含英文技术词、不含错误码、不含地址（§58）；
 * 2. **不惊悚**：通道抖动是常态，学生正在共享整块屏幕，弹一个红色"连接失败"
 *    只会让他去拔网线。这里全部是中性偏灰的说明；
 * 3. **不撒谎**：没连上时不能显示"已连接"，也不能暗示"一切正常"——
 *    这几句话的存在就是为了让"我现在看到的可能不是最新的"这件事可见。
 */

/** 与 @classwatch/ui 的 StatusTone 词表对齐（语义色由视图消费）。 */
export type RealtimeTone = 'open' | 'warning' | 'neutral'

export interface RealtimeStatusDisplay {
  /** 一行状态标签（会话页与列表页共用）。 */
  label: string
  tone: RealtimeTone
  /**
   * 需要额外解释时的第二句；已连接时为 null（正常状态不需要解释）。
   */
  hint: string | null
}

/**
 * `authFailed` 与 `state` 分开传：它是"停止重连"的原因判定，
 * 不是第四种连接状态（冻结词表只有 connecting / open / closed）。
 *
 * `wasConnected` 用来区分两句话："第一次还没连上"与"连上过又断了"。
 * 后者才叫**重连**——对正在共享屏幕的学生来说，"刚掉线、正在重连"与
 * "一直连不上"需要做的事并不一样（前者等一会儿，后者要刷新页面）。
 */
export function describeRealtimeStatus(
  state: RealtimeConnectionState,
  options: { authFailed: boolean; wasConnected: boolean },
): RealtimeStatusDisplay {
  if (state === 'open') {
    return { label: '实时通道已连接', tone: 'open', hint: null }
  }
  if (options.authFailed) {
    return {
      label: '实时通道未建立',
      tone: 'warning',
      hint: '课堂状态改为定期刷新。若长时间如此，请刷新页面重新登录。',
    }
  }
  if (state === 'connecting') {
    return options.wasConnected
      ? {
          label: '正在重连实时通道…',
          tone: 'neutral',
          hint: '课堂状态仍会定期刷新；老师关闭课堂时的提示可能稍有延迟。',
        }
      : {
          label: '正在连接实时通道…',
          tone: 'neutral',
          hint: '连接期间课堂状态仍会定期刷新，提示可能稍有延迟。',
        }
  }
  return {
    label: '实时通道已断开，正在重连…',
    tone: 'neutral',
    hint: '课堂状态仍会定期刷新；老师关闭课堂时的提示可能稍有延迟。',
  }
}
