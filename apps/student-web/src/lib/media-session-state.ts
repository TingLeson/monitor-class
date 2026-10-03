/**
 * 课堂会话的状态词表（§22 / §49 / §56）。
 *
 * WHY 把"阶段"与"文案"放在 lib 而不是 store / 视图里：
 * - store 关心的是**迁移**（什么时候从 online 变成 screen-lost），
 *   视图关心的是**渲染**，而"这个阶段对学生意味着什么"是第三件事；
 * - 两者都需要同一份词表（视图显示，store 的失败态也要有中文 message），
 *   写在视图里会让 store 的测试只能断言一个英文枚举。
 *
 * 这一层**只是**前端 UI 状态，不是 §12 的服务端 Session 状态（CONNECTING /
 * ONLINE / SCREEN_LOST / ...）。两者刻意不共用类型：服务端状态以后端为准
 * （Phase 8 起由 WebSocket 推送），前端阶段只是"我这一端现在到哪一步了"，
 * 一个刷新就会归零。
 */

import type { RealtimeConnectionState } from '@classwatch/api-client'

/**
 * 前端会话阶段。
 *
 * | 阶段 | 含义 |
 * | --- | --- |
 * | `idle` | 没有会话（还没 prepare，或已彻底清理） |
 * | `prepared` | join 成功，凭据与屏幕轨道都在内存里，等待会话页连接 |
 * | `connecting` | 正在连媒体服务器 / 发布屏幕轨道 |
 * | `online` | 已进入课堂：屏幕轨道已发布（§21 的不变量在前端这一侧的体现） |
 * | `screen-lost` | 屏幕共享中断，等待学生重新共享（§22） |
 * | `media-error` | 媒体连接或发布失败，可重试（重试会重新 join 拿新 token） |
 * | `closed` | 老师关闭了课堂（§49） |
 * | `left` | 学生主动离开（或页面卸载时已尽力断开） |
 * | `no-session` | 内存里没有会话凭据（刷新、直接输入 URL） |
 */
export type MediaSessionPhase =
  | 'idle'
  | 'prepared'
  | 'connecting'
  | 'online'
  | 'screen-lost'
  | 'media-error'
  | 'closed'
  | 'left'
  | 'no-session'

/**
 * 媒体失败的类别。
 *
 * WHY 不用 §58 的错误码：§58 的前端本地错误码全部来自**浏览器媒体 API**
 * （权限、共享面、轨道结束），而"连不上 SFU""publish 被拒"是媒体**链路**问题，
 * 不在那张表里。为了避免为了让类型好看而往契约里塞新码，这里用 app 内部的
 * 类别 + 固定中文文案；它们不会出现在任何接口上。
 */
export type MediaFailureKind = 'connect' | 'publish' | 'disconnected' | 'rejoin'

export interface MediaFailure {
  kind: MediaFailureKind
  /** 可直接展示给学生的中文（已包含"下一步做什么"）。 */
  message: string
}

/**
 * 每一类失败的文案。
 *
 * 三条约束（与 `screen-capture-messages.ts` 相同）：
 * 1. 不出现英文技术信息、不出现错误码、不出现原始报文（§58）；
 * 2. 必须给出下一步动作（重试 / 重新共享 / 回课堂列表）；
 * 3. 不暗示"媒体成功"：`publish` 失败时，老师那端看到的就是一个没有屏幕的学生，
 *    文案不能说成"稍等就好"。
 */
const FAILURE_MESSAGES: Record<MediaFailureKind, string> = {
  connect: '无法连接课堂的媒体服务器。请检查网络后重试；重试会重新获取一次课堂凭据。',
  publish: '屏幕共享没有成功发布到课堂。请重试；多次失败请离开课堂后重新进入。',
  disconnected: '与课堂的媒体连接已经中断。请重试重新进入课堂，或离开后重新进入。',
  rejoin: '重新进入课堂失败。请返回我的课堂，重新共享整个屏幕后再进入。',
}

export function describeMediaFailure(kind: MediaFailureKind): string {
  return FAILURE_MESSAGES[kind]
}

/** 会话阶段在界面上呈现的语义色（与 @classwatch/ui 的 StatusTone 对齐）。 */
export type SessionPhaseTone = 'open' | 'warning' | 'danger' | 'neutral' | 'closed'

export interface SessionPhaseDisplay {
  /** 「当前状态：」后面那一行（§56 原文用词）。 */
  label: string
  tone: SessionPhaseTone
}

/**
 * §56 的「当前状态」三句话 + 其余阶段。
 *
 * `screen-lost` 用 §22 的原文「⚠ 已停止屏幕共享」，一个字都不改：
 * 这句话在 PreJoin、会话页、老师端三处必须一致，学生才会意识到"是同一件事"。
 */
const PHASE_DISPLAY: Record<MediaSessionPhase, SessionPhaseDisplay> = {
  idle: { label: '连接中…', tone: 'neutral' },
  prepared: { label: '连接中…', tone: 'neutral' },
  connecting: { label: '连接中…', tone: 'neutral' },
  online: { label: '已进入课堂', tone: 'open' },
  'screen-lost': { label: '⚠ 已停止屏幕共享', tone: 'danger' },
  'media-error': { label: '媒体连接异常', tone: 'danger' },
  closed: { label: '课堂已结束', tone: 'closed' },
  left: { label: '已离开课堂', tone: 'neutral' },
  'no-session': { label: '会话已失效', tone: 'warning' },
}

export function describeSessionPhase(phase: MediaSessionPhase): SessionPhaseDisplay {
  return PHASE_DISPLAY[phase]
}

/**
 * 课堂状态的**兜底轮询**间隔（§49 学生侧；Phase 8 起实时通道才是主路径）。
 *
 * WHY 还要留轮询：WebSocket 会断（网络切换、服务端重启、代理超时）。断线期间
 * 学生必须仍然能发现"老师已经关闭本课堂"——否则他会一直共享着一块没人看的屏幕，
 * 而这是本系统里学生侧最糟糕的状态。§47 的实时通道是**快**，兜底轮询负责**不漏**。
 *
 * 两档而不是一档：
 * - 实时通道正常时 60 秒一次：事件已经覆盖了几乎所有情况，轮询只用来兜住
 *   "服务端漏发/事件丢失"这种罕见情况，频率必须足够低才不会变成事实上的轮询系统；
 * - 实时通道断开时收紧到 30 秒：这段时间里**只有**轮询能发现关课，
 *   60 秒的延迟对"还在被共享屏幕"的学生来说太久了。
 */
export const CLASSROOM_FALLBACK_POLL_MS = 60_000
export const CLASSROOM_DEGRADED_POLL_MS = 30_000

/** 按实时通道状态挑一个兜底间隔（`open` 以外的一切都按降级处理）。 */
export function classroomFallbackPollMs(realtime: RealtimeConnectionState): number {
  return realtime === 'open' ? CLASSROOM_FALLBACK_POLL_MS : CLASSROOM_DEGRADED_POLL_MS
}
