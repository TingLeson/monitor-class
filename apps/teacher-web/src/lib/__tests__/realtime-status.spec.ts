import { describe, expect, it } from 'vitest'
import { describeRealtimeStatus } from '../realtime-status.ts'

/**
 * 老师端实时状态文案测试（§47 / §51 / §58）。
 *
 * 监督墙的全部价值是"老师看到的就是现在发生的"，所以这一层的每句话都必须
 * 在**没连上**的时候说出来，并且给出可执行的动作（重新加载 / 刷新页面）。
 * 反过来，连上了就不要常驻一句废话占地方。
 */
describe('老师端实时状态文案', () => {
  it('已连接：不加解释', () => {
    expect(describeRealtimeStatus('open', { authFailed: false, wasConnected: true })).toEqual({
      label: '实时状态已连接',
      tone: 'open',
      hint: null,
    })
  })

  it('首次连接与断线重连说不同的话', () => {
    const first = describeRealtimeStatus('connecting', { authFailed: false, wasConnected: false })
    const again = describeRealtimeStatus('connecting', { authFailed: false, wasConnected: true })

    expect(first.label).toContain('正在连接')
    expect(again.label).toContain('重连')
  })

  it('未连接时必须说出"监督数据可能不是最新"并给出动作', () => {
    for (const state of ['connecting', 'closed'] as const) {
      const display = describeRealtimeStatus(state, { authFailed: false, wasConnected: true })
      expect(display.hint).toContain('可能不是最新')
      expect(display.hint).toContain('重新加载')
    }
  })

  it('握手被拒：说"未建立 + 可能需要重新登录"，而不是"你被登出了"', () => {
    const display = describeRealtimeStatus('closed', { authFailed: true, wasConnected: false })

    expect(display.tone).toBe('warning')
    expect(display.hint).toContain('重新登录')
    expect(display.hint).toContain('可能不是最新')
  })

  it('§58：不出现英文技术词与错误码', () => {
    for (const state of ['connecting', 'open', 'closed'] as const) {
      for (const authFailed of [false, true]) {
        const display = describeRealtimeStatus(state, { authFailed, wasConnected: true })
        expect(`${display.label}${display.hint ?? ''}`).not.toMatch(/[A-Z_]{4,}/)
      }
    }
  })
})
