import { describe, expect, it } from 'vitest'
import { ApiError } from '@classwatch/api-client'
import {
  makeLeftStudent,
  makeMonitorStudent,
  makeNotJoinedStudent,
} from '../../__tests__/monitor-fixtures.ts'
import {
  canStartPrivateTalk,
  describeTalkActionText,
  describeTalkText,
  talkBlockedReason,
  toPrivateTalkFailure,
} from '../private-talk.ts'

/**
 * 私密语音词表与前置判断测试（§31 / §58）。
 *
 * 这一份盯住的是"老师能做什么"而不是"界面长什么样"：
 * 未进入 / 已离开的学生必须**在点击之前**就被拦住，并且给出一句能照做的话；
 * 每个错误码都要有一句可执行的中文（§58 的客户端按 code 分支）。
 */

describe('私密语音的前置判断（§31）', () => {
  it('在线（含屏幕中断）的学生可以发起', () => {
    expect(canStartPrivateTalk(makeMonitorStudent())).toBe(true)
    expect(canStartPrivateTalk(makeMonitorStudent({ sessionStatus: 'SCREEN_LOST' }))).toBe(true)
    expect(talkBlockedReason(makeMonitorStudent())).toBeNull()
  })

  it('未进入课堂（sessionId === null）→ 不能发起，并说明原因', () => {
    const student = makeNotJoinedStudent()

    expect(canStartPrivateTalk(student)).toBe(false)
    expect(talkBlockedReason(student)).toContain('尚未进入课堂')
  })

  it('已离开 / 连接断开 → 同样不能发起，但原因不同（老师能做的事不同）', () => {
    expect(talkBlockedReason(makeLeftStudent())).toContain('已经结束')
    expect(talkBlockedReason(makeMonitorStudent({ sessionStatus: 'DISCONNECTED' }))).toContain(
      '连接已断开',
    )
    expect(talkBlockedReason(makeMonitorStudent({ sessionStatus: 'CONNECTING' }))).toContain(
      '正在进入课堂',
    )
  })
})

describe('私密语音的失败文案（§58）', () => {
  it('TEACHER_MIC_REQUIRED → "请先开启你的麦克风" + micRequired', () => {
    const failure = toPrivateTalkFailure(
      new ApiError({ code: 'TEACHER_MIC_REQUIRED', message: 'mic', status: 409 }),
    )

    expect(failure.micRequired).toBe(true)
    expect(failure.message).toContain('请先开启你的麦克风')
  })

  it('PRIVATE_TALK_UNAVAILABLE → 说明"他不在课堂中"以及可以再试', () => {
    const failure = toPrivateTalkFailure(
      new ApiError({ code: 'PRIVATE_TALK_UNAVAILABLE', message: 'unavailable', status: 409 }),
    )

    expect(failure.micRequired).toBe(false)
    expect(failure.message).toContain('不在课堂中')
  })

  it('未知失败（本地异常 / 传输失败）走通用文案，且不假装需要开麦', () => {
    const failure = toPrivateTalkFailure(new Error('boom'))

    expect(failure.code).toBeNull()
    expect(failure.micRequired).toBe(false)
    expect(failure.message.length).toBeGreaterThan(0)
  })
})

describe('私密语音的界面文案（§31）', () => {
  it('页头与按钮共用同一句话，只有结尾的动作不同', () => {
    expect(describeTalkText('张三')).toBe('正在与张三语音沟通')
    expect(describeTalkActionText('张三')).toBe('正在与张三语音沟通 · 结束')
  })
})
