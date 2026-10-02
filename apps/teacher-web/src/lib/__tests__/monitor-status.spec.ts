import type { ConnectionQuality, MonitorStudent } from '@classwatch/shared-types'
import { describe, expect, it } from 'vitest'
import {
  makeConnectingStudent,
  makeDisconnectedStudent,
  makeLeftStudent,
  makeMonitorStudent,
  makeNotJoinedStudent,
  makeRoomClosedStudent,
  makeScreenLostStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  TILE_BODY_HINT,
  TILE_BODY_TEXT,
  deriveMonitorTileState,
  describeConnectionHint,
  describeMonitorBadge,
  describeNetworkLevel,
  describeTileBody,
  isStudentEntered,
  shouldSubscribeScreen,
} from '../monitor-status.ts'

/**
 * 监督卡片状态映射测试（§12 / §22 / §29 / §30 / §51 / §52）。
 *
 * 这一层是"业务状态怎么显示给老师"的唯一出口，因此断言的都是**不能出错的分支**：
 * 把屏幕中断显示成"正常"，就是监督系统最严重的谎报。
 */

/** 六档落库状态 + 未进入（Phase 7 的冻结契约允许 sessionStatus 为 null）。 */
const BADGE_CASES: { name: string; student: MonitorStudent; emoji: string; label: string }[] = [
  { name: 'ONLINE + 屏幕在发布', student: makeMonitorStudent(), emoji: '🟢', label: '正常' },
  {
    name: 'ONLINE 但屏幕没在发布',
    student: makeMonitorStudent({ screen: { active: false } }),
    emoji: '🔴',
    label: '屏幕中断',
  },
  { name: 'SCREEN_LOST', student: makeScreenLostStudent(), emoji: '🔴', label: '屏幕中断' },
  { name: 'CONNECTING', student: makeConnectingStudent(), emoji: '🟡', label: '连接中' },
  { name: 'DISCONNECTED', student: makeDisconnectedStudent(), emoji: '⚪', label: '已断开' },
  { name: 'LEFT', student: makeLeftStudent(), emoji: '⚫', label: '已离开' },
  { name: 'ROOM_CLOSED', student: makeRoomClosedStudent(), emoji: '⚫', label: '已离开' },
  // 最重要的一档：后端返回"名单里有他、但没有会话"，界面必须一眼看得出"没进来"。
  {
    name: '未进入（sessionStatus = null）',
    student: makeNotJoinedStudent(),
    emoji: '⚪',
    label: '未进入',
  },
]

describe('监督徽章映射（§29 / §51）', () => {
  it.each(BADGE_CASES)('$name → $emoji $label', ({ student, emoji, label }) => {
    expect(describeMonitorBadge(student)).toMatchObject({ emoji, label })
  })

  it('六档状态 + null 一共七种组合，没有漏掉任何一档', () => {
    // 每个夹具都必须落在上面那张表里：新增一档落库状态却忘记映射时，这条断言会失败。
    const labels = BADGE_CASES.map((entry) => describeMonitorBadge(entry.student).label)
    expect(labels).toHaveLength(8)
    expect(new Set(labels)).toEqual(
      new Set(['正常', '屏幕中断', '连接中', '已断开', '已离开', '未进入']),
    )
  })

  it('语义色分档正确：正常=positive / 中断=danger / 连接中=attention / 其余=muted', () => {
    expect(describeMonitorBadge(makeMonitorStudent()).tone).toBe('positive')
    expect(describeMonitorBadge(makeScreenLostStudent()).tone).toBe('danger')
    expect(describeMonitorBadge(makeConnectingStudent()).tone).toBe('attention')
    for (const student of [makeDisconnectedStudent(), makeLeftStudent(), makeNotJoinedStudent()]) {
      expect(describeMonitorBadge(student).tone).toBe('muted')
    }
  })
})

describe('派生状态（§29 的 MonitorTileState）', () => {
  it('六个派生值与落库状态的对应关系是逐字的', () => {
    expect(deriveMonitorTileState(makeMonitorStudent())).toBe('NORMAL')
    expect(deriveMonitorTileState(makeScreenLostStudent())).toBe('SCREEN_LOST')
    expect(deriveMonitorTileState(makeConnectingStudent())).toBe('CONNECTING')
    expect(deriveMonitorTileState(makeDisconnectedStudent())).toBe('DISCONNECTED')
    expect(deriveMonitorTileState(makeLeftStudent())).toBe('LEFT')
    expect(deriveMonitorTileState(makeRoomClosedStudent())).toBe('LEFT')
    expect(deriveMonitorTileState(makeNotJoinedStudent())).toBe('NOT_JOINED')
  })

  it('sessionStatus 为 null 时永远优先判"未进入"（哪怕 screen.active 是脏数据）', () => {
    // 没有会话却声称屏幕在发布：契约被破坏的脏行。宁可显示"未进入"，
    // 也不能让它进到"该订阅"的集合里（没有 identity 根本订不到东西）。
    const dirty = makeNotJoinedStudent({ screen: { active: true } })

    expect(deriveMonitorTileState(dirty)).toBe('NOT_JOINED')
    expect(shouldSubscribeScreen(dirty)).toBe(false)
  })
})

describe('已进入判据（§29 头部计数与徽章共用）', () => {
  it('在线且有会话才算已进入', () => {
    expect(isStudentEntered(makeMonitorStudent())).toBe(true)
    // 屏幕中断的学生人还在课堂里，只是没有画面。
    expect(isStudentEntered(makeScreenLostStudent())).toBe(true)
  })

  it('连接中 / 已断开 / 已离开 / 未进入都不算', () => {
    for (const student of [
      makeConnectingStudent(),
      makeDisconnectedStudent(),
      makeLeftStudent(),
      makeRoomClosedStudent(),
      makeNotJoinedStudent(),
    ]) {
      expect(isStudentEntered(student)).toBe(false)
    }
  })

  it('有会话但没有 identity 的行不算已进入（订阅必须拿得到 identity）', () => {
    expect(isStudentEntered(makeMonitorStudent({ sessionId: '' }))).toBe(false)
  })
})

describe('订阅判据（§52 的业务侧）', () => {
  it('只有在线 + 屏幕在发布 + 有 sessionId 的学生才订阅', () => {
    expect(shouldSubscribeScreen(makeMonitorStudent())).toBe(true)
    expect(shouldSubscribeScreen(makeScreenLostStudent())).toBe(false)
    expect(shouldSubscribeScreen(makeNotJoinedStudent())).toBe(false)
    expect(shouldSubscribeScreen(makeMonitorStudent({ screen: { active: false } }))).toBe(false)
    expect(shouldSubscribeScreen(makeMonitorStudent({ sessionId: null }))).toBe(false)
    expect(shouldSubscribeScreen(makeMonitorStudent({ sessionId: '' }))).toBe(false)
  })

  it('§52：已经断开的卡片不订阅，哪怕后端这一轮还说着"屏幕在发布"', () => {
    // 只判 screen.active 会留一个窗口期：后端把学生标成 DISCONNECTED 之后，
    // screen.active 可能还是 true，那几秒里会挂着一条永远不会有画面的下行。
    const stale = makeDisconnectedStudent({ screen: { active: true } })

    expect(shouldSubscribeScreen(stale)).toBe(false)
  })

  it('连接中的卡片也不订阅（媒体还没连上，订阅必然拿不到轨道）', () => {
    expect(shouldSubscribeScreen(makeConnectingStudent({ screen: { active: true } }))).toBe(false)
  })

  it('可见性是 store 的另一层筛子，这里不做判断（两个都在共享的学生都通过业务侧判据）', () => {
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

  it('订阅到位 → live（显示 <video>），与业务状态无关', () => {
    expect(
      describeTileBody(makeMonitorStudent(), { state: 'subscribed', hasSubscription: true }),
    ).toBe('live')
  })

  it('订阅失败 → failed（给重试入口，而不是一直转圈）', () => {
    expect(
      describeTileBody(makeMonitorStudent(), { state: 'failed', hasSubscription: false }),
    ).toBe('failed')
  })

  it('六种"没有画面"各有各的说法，不能都退化成一句"未连接"', () => {
    const none = { state: 'none', hasSubscription: false } as const
    expect(describeTileBody(makeScreenLostStudent(), none)).toBe('lost')
    expect(describeTileBody(makeConnectingStudent(), none)).toBe('connecting')
    expect(describeTileBody(makeDisconnectedStudent(), none)).toBe('disconnected')
    expect(describeTileBody(makeLeftStudent(), none)).toBe('left')
    expect(describeTileBody(makeNotJoinedStudent(), none)).toBe('notJoined')

    expect(TILE_BODY_TEXT.lost).toBe('等待共享')
    expect(TILE_BODY_TEXT.notJoined).toBe('未进入课堂')
    expect(TILE_BODY_TEXT.disconnected).toBe('连接已断开')
    expect(TILE_BODY_TEXT.left).toBe('已离开课堂')
    expect(TILE_BODY_TEXT.connecting).toBe('正在连接…')
  })

  it('"未进入"必须带上原因，否则老师会以为是画面没加载出来', () => {
    expect(TILE_BODY_HINT.notJoined).toContain('尚未进入')
  })
})

describe('连接提示（§29 页脚 / §30 网络行）', () => {
  it('页脚用短词，且 UNKNOWN 不会被说成"正常"', () => {
    expect(describeConnectionHint('GOOD')).toBe('正常')
    expect(describeConnectionHint('FAIR')).toBe('一般')
    expect(describeConnectionHint('POOR')).toBe('较差')
    expect(describeConnectionHint('UNKNOWN')).toBe('未知')
  })

  it('Focus 的 Network 行沿用 §30 线框图的取值', () => {
    expect(describeNetworkLevel('GOOD')).toBe('Good')
    expect(describeNetworkLevel('FAIR')).toBe('Fair')
    expect(describeNetworkLevel('POOR')).toBe('Poor')
    // 未知不是一个"等级"，措辞上必须与三档区分开。
    expect(describeNetworkLevel('UNKNOWN')).toBe('未知')
  })

  it('四档取值全部有对应文案（新增等级时这条会失败）', () => {
    for (const quality of ['GOOD', 'FAIR', 'POOR', 'UNKNOWN'] as ConnectionQuality[]) {
      expect(describeConnectionHint(quality)).not.toBe('')
      expect(describeNetworkLevel(quality)).not.toBe('')
    }
  })
})
