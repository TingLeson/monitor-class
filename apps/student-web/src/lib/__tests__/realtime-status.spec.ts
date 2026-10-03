import { describe, expect, it } from 'vitest'
import { describeRealtimeStatus } from '../realtime-status.ts'

/**
 * 实时通道文案测试（§47 / §58 / §74）。
 *
 * 这一层的价值全在"不漏项、不撒谎"：
 * - 三种连接状态 + "握手被拒"这一种判定必须都有话说；
 * - 未连接时**必须**明说状态可能不是最新，并给出下一步动作；
 * - 文案里不许出现英文技术词与错误码（§58 的措辞纪律）。
 */
describe('学生端实时通道文案', () => {
  it('已连接：不加解释（正常状态不需要占地方）', () => {
    expect(describeRealtimeStatus('open', { authFailed: false, wasConnected: true })).toEqual({
      label: '实时通道已连接',
      tone: 'open',
      hint: null,
    })
  })

  it('首次连接与断线重连说不同的话（"正在连接"≠"正在重连"）', () => {
    const first = describeRealtimeStatus('connecting', { authFailed: false, wasConnected: false })
    const again = describeRealtimeStatus('connecting', { authFailed: false, wasConnected: true })
    const closed = describeRealtimeStatus('closed', { authFailed: false, wasConnected: true })

    expect(first.label).toContain('正在连接')
    expect(again.label).toContain('重连')
    expect(closed.label).toContain('重连')
    expect(first.hint).toBeTruthy()
    expect(again.hint).toBeTruthy()
    expect(closed.hint).toBeTruthy()
  })

  it('握手被拒：说"未建立 + 可能需要重新登录"，而不是"你被登出了"', () => {
    const display = describeRealtimeStatus('closed', { authFailed: true, wasConnected: false })

    expect(display.tone).toBe('warning')
    expect(display.hint).toContain('重新登录')
    expect(display.hint).not.toContain('已登出')
  })

  it('§58：不出现英文技术词与错误码，且每句都给出下一步', () => {
    const states = ['connecting', 'open', 'closed'] as const
    for (const state of states) {
      for (const authFailed of [false, true]) {
        const display = describeRealtimeStatus(state, { authFailed, wasConnected: true })
        const text = `${display.label}${display.hint ?? ''}`
        expect(display.label.length).toBeGreaterThan(0)
        expect(text).not.toMatch(/[A-Z_]{4,}/)
        // 未连接时必须给出可执行的动作（等 / 刷新页面），不能只说"出问题了"。
        if (state !== 'open') expect(text).toMatch(/刷新|重连|连接/)
      }
    }
  })
})
