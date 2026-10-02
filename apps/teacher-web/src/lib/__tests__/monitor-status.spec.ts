import { describe, expect, it } from 'vitest'
import {
  makeMonitorStudent,
  makeOfflineStudent,
  makeScreenLostStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  TILE_BODY_TEXT,
  describeMonitorBadge,
  describeTileBody,
  shouldSubscribeScreen,
} from '../monitor-status.ts'

/**
 * 监督卡片状态映射测试（§22 / §29 / §51 / §52）。
 *
 * 这一层是"业务状态怎么显示给老师"的唯一出口，因此断言的都是**不能出错的分支**：
 * 把屏幕中断显示成"正常"，就是监督系统最严重的谎报。
 */
describe('监督徽章映射（§29 / §51）', () => {
  it('ONLINE + 屏幕在发布 → 🟢 正常', () => {
    expect(describeMonitorBadge(makeMonitorStudent())).toEqual({
      emoji: '🟢',
      label: '正常',
      tone: 'positive',
    })
  })

  it('SCREEN_LOST → 🔴 屏幕中断（§22 的专用状态，优先于 screen.active 判断）', () => {
    expect(describeMonitorBadge(makeScreenLostStudent())).toEqual({
      emoji: '🔴',
      label: '屏幕中断',
      tone: 'danger',
    })
  })

  it('ONLINE 但 screen.active=false → 同样按屏幕中断处理（§21 的不变量被破坏时不许说"正常"）', () => {
    const student = makeMonitorStudent({ screen: { active: false } })

    expect(describeMonitorBadge(student).label).toBe('屏幕中断')
  })

  it('未连接（从未加入 / 掉线 / 已离开 / 课堂已结束）→ ⚪ 未连接', () => {
    expect(describeMonitorBadge(makeOfflineStudent()).label).toBe('未连接')
    // 这几档不是"屏幕中断"：学生根本不在课堂上，老师要做的事完全不同。
    for (const status of ['CONNECTING', 'DISCONNECTED', 'LEFT', 'ROOM_CLOSED'] as const) {
      expect(describeMonitorBadge(makeMonitorStudent({ sessionStatus: status })).label).toBe(
        '未连接',
      )
    }
  })
})

describe('订阅判据（§52）', () => {
  it('只有 screen.active 且有 sessionId 的学生才订阅', () => {
    expect(shouldSubscribeScreen(makeMonitorStudent())).toBe(true)
    expect(shouldSubscribeScreen(makeScreenLostStudent())).toBe(false)
    expect(shouldSubscribeScreen(makeOfflineStudent())).toBe(false)
    // screen 活着但没有会话可对应（后端状态异常）：没有 identity 可订，跳过。
    expect(shouldSubscribeScreen(makeMonitorStudent({ sessionId: null }))).toBe(false)
    expect(shouldSubscribeScreen(makeMonitorStudent({ sessionId: '' }))).toBe(false)
  })

  it('本 Phase 不按可见性筛选（那是 Phase 7 的动态订阅优化）', () => {
    // 两个都在共享的学生都会被订阅：Phase 6 只保证"不多订阅、不重复订阅"。
    const a = makeMonitorStudent({ studentId: 'a', sessionId: 'session-a' })
    const b = makeMonitorStudent({ studentId: 'b', sessionId: 'session-b' })
    expect([a, b].filter(shouldSubscribeScreen)).toHaveLength(2)
  })
})

describe('卡片主体（§51 的状态合成）', () => {
  it('业务说该有画面、媒体还没到 → 正在订阅画面…', () => {
    expect(
      describeTileBody(makeMonitorStudent(), { state: 'pending', hasSubscription: false }),
    ).toBe('waiting')
    expect(TILE_BODY_TEXT.waiting).toBe('正在订阅画面…')
  })

  it('订阅到位 → live（显示 <video>）', () => {
    expect(
      describeTileBody(makeMonitorStudent(), { state: 'subscribed', hasSubscription: true }),
    ).toBe('live')
  })

  it('订阅失败 → failed（给重试入口，而不是一直转圈）', () => {
    expect(
      describeTileBody(makeMonitorStudent(), { state: 'failed', hasSubscription: false }),
    ).toBe('failed')
  })

  it('屏幕中断 / 未连接分别给出不同的占位文案', () => {
    expect(
      describeTileBody(makeScreenLostStudent(), { state: 'none', hasSubscription: false }),
    ).toBe('lost')
    expect(describeTileBody(makeOfflineStudent(), { state: 'none', hasSubscription: false })).toBe(
      'offline',
    )
    expect(TILE_BODY_TEXT.lost).toBe('等待共享')
    expect(TILE_BODY_TEXT.offline).toBe('未连接')
  })
})
