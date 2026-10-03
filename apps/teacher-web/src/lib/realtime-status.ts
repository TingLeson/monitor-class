import type { RealtimeConnectionState } from '@classwatch/api-client'

/**
 * 实时通道状态在**老师端**的文案（§47 / §51）。
 *
 * 与学生端的措辞刻意不同：学生关心"课堂还开着吗"，老师关心的是
 * **"我现在看到的状态是不是最新的"**——监督墙一旦显示陈旧状态，
 * 老师就可能对着一个已经离开的学生做判断（§51 的整个 DTO 设计就是为了防这件事）。
 *
 * 三条约束（与 `classroom-error.ts` 的文案纪律相同）：
 * 1. 不含英文技术词、不含错误码、不含地址（§58）；
 * 2. 不惊悚：通道抖动是常态，"实时状态短暂中断"不等于课堂出事了；
 * 3. 不撒谎：断线时必须说出"可能不是最新"，并给出可执行的动作（重新加载）。
 */

export type RealtimeTone = 'open' | 'warning' | 'neutral'

export interface RealtimeStatusDisplay {
  label: string
  tone: RealtimeTone
  /** 需要额外解释时的第二句；已连接时为 null。 */
  hint: string | null
}

/**
 * `authFailed` 是"停止重连"的原因判定（浏览器不暴露握手失败的状态码，
 * 见 api-client 的说明），不是第四种连接状态——冻结词表只有 connecting / open / closed。
 *
 * `wasConnected` 区分"首次还没连上"与"连上过又断了"：后者才该说"重连"，
 * 而"可能不是最新"这句话两者都必须说——监督墙的价值就是"看到的是现在"。
 */
export function describeRealtimeStatus(
  state: RealtimeConnectionState,
  options: { authFailed: boolean; wasConnected: boolean },
): RealtimeStatusDisplay {
  if (state === 'open') {
    return { label: '实时状态已连接', tone: 'open', hint: null }
  }
  if (options.authFailed) {
    return {
      label: '实时状态未建立',
      tone: 'warning',
      hint: '监督数据可能不是最新。若长时间如此，请刷新页面重新登录。',
    }
  }
  if (state === 'connecting' && !options.wasConnected) {
    return {
      label: '正在连接实时状态…',
      tone: 'neutral',
      hint: '连接期间监督数据可能稍有延迟。',
    }
  }
  return {
    label: '实时状态已断开，正在重连…',
    tone: 'neutral',
    hint: '监督数据可能不是最新，可点「重新加载」取一次快照。',
  }
}
