import { describe, expect, it } from 'vitest'
import { describeConnectionQuality } from '../connection-quality.ts'
import {
  CLASSROOM_DEGRADED_POLL_MS,
  CLASSROOM_FALLBACK_POLL_MS,
  classroomFallbackPollMs,
  describeMediaFailure,
  describeSessionPhase,
  type MediaFailureKind,
  type MediaSessionPhase,
} from '../media-session-state.ts'

/**
 * 词表测试（§22 / §49 / §52 / §56）。
 *
 * 这些映射的价值全在"不漏项、不改口"：少一个等级会让界面出现空白，
 * 改一句 §22 的原文会让同类提示在不同页面上说法不一致。
 */
describe('连接质量词表', () => {
  it('五档 ConnectionQuality 都有中文 + 语义色（§56 的网络行）', () => {
    expect(describeConnectionQuality('excellent')).toMatchObject({ label: '极佳', tone: 'open' })
    expect(describeConnectionQuality('good')).toMatchObject({ label: '良好', tone: 'open' })
    expect(describeConnectionQuality('poor')).toMatchObject({ label: '较差', tone: 'warning' })
    expect(describeConnectionQuality('lost')).toMatchObject({ label: '已断开', tone: 'danger' })
    expect(describeConnectionQuality('unknown')).toMatchObject({ label: '未知', tone: 'neutral' })
  })

  it('"较差"与"已断开"必须是两种说法：它们的下一步动作完全不同（§52）', () => {
    expect(describeConnectionQuality('poor').hint).not.toBe(describeConnectionQuality('lost').hint)
    // 断了就要说"重新进入课堂"，而不是"可能会卡"。
    expect(describeConnectionQuality('lost').hint).toContain('重新进入课堂')
  })
})

describe('会话阶段词表', () => {
  it('§56 的三句话逐字对应：连接中… / 已进入课堂 / ⚠ 已停止屏幕共享', () => {
    expect(describeSessionPhase('connecting').label).toBe('连接中…')
    expect(describeSessionPhase('prepared').label).toBe('连接中…')
    expect(describeSessionPhase('online').label).toBe('已进入课堂')
    // §22 的原文，一个字都不能改：它与 PreJoin、老师端必须是同一句话。
    expect(describeSessionPhase('screen-lost').label).toBe('⚠ 已停止屏幕共享')
  })

  it('每一个阶段都有文案（新增阶段而忘记写文案会让界面出现空白）', () => {
    const phases: MediaSessionPhase[] = [
      'idle',
      'prepared',
      'connecting',
      'online',
      'screen-lost',
      'media-error',
      'closed',
      'left',
      'no-session',
    ]
    for (const phase of phases) {
      expect(describeSessionPhase(phase).label.length).toBeGreaterThan(0)
    }
  })

  it('§58：失败文案只讲中文的下一步动作，不含错误码与英文技术词', () => {
    const kinds: MediaFailureKind[] = ['connect', 'publish', 'disconnected', 'rejoin']
    for (const kind of kinds) {
      const message = describeMediaFailure(kind)
      expect(message.length).toBeGreaterThan(0)
      expect(message).not.toMatch(/[A-Z_]{4,}/)
      expect(message).toContain('请')
    }
  })
})

describe('课堂状态兜底轮询间隔（§47/§49）', () => {
  it('实时通道正常时是低频兜底（≥ 60 秒），绝不退化成"事实上的轮询"', () => {
    expect(CLASSROOM_FALLBACK_POLL_MS).toBeGreaterThanOrEqual(60_000)
    expect(classroomFallbackPollMs('open')).toBe(CLASSROOM_FALLBACK_POLL_MS)
  })

  it('实时通道断开时收紧：这段时间里轮询是唯一能发现"课堂已关闭"的手段', () => {
    expect(CLASSROOM_DEGRADED_POLL_MS).toBeLessThan(CLASSROOM_FALLBACK_POLL_MS)
    expect(classroomFallbackPollMs('connecting')).toBe(CLASSROOM_DEGRADED_POLL_MS)
    expect(classroomFallbackPollMs('closed')).toBe(CLASSROOM_DEGRADED_POLL_MS)
  })
})
