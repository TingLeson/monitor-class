import { ApiError } from '@classwatch/api-client'
import { API_ERROR_MESSAGES } from '@classwatch/shared-types'
import { describe, expect, it } from 'vitest'
import { describeStudentClassroomError, isNotAssigned } from '../student-classroom-error'

/**
 * 学生端错误文案映射测试（§58；docs/frontend/student.md §5）。
 *
 * 这一层唯一要守住的是"学生看到的那句话"：既不能是一段后端报文，
 * 也不能是一句没有下一步动作的"操作失败"。
 */
describe('学生端课堂错误文案', () => {
  it('五个本 Phase 会遇到的错误码各自给出可执行的中文文案', () => {
    expect(
      describeStudentClassroomError(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 })),
    ).toBe('你不在这个课堂的名单里，请联系老师确认。')
    expect(
      describeStudentClassroomError(new ApiError({ code: 'AUTH_REQUIRED', status: 401 })),
    ).toBe('登录状态已失效，请重新登录。')
    expect(
      describeStudentClassroomError(new ApiError({ code: 'ACCOUNT_DISABLED', status: 403 })),
    ).toBe('账号已被停用，请联系管理员。')
    expect(describeStudentClassroomError(new ApiError({ code: 'RATE_LIMITED', status: 429 }))).toBe(
      '尝试过于频繁，请稍后再试。',
    )
    expect(describeStudentClassroomError(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))).toBe(
      '网络连接失败，请检查网络后重试。',
    )
  })

  it('未登记的错误码走共享表的通用文案，不编造原因', () => {
    // CLASSROOM_NOT_OWNER 属于老师端的码，学生端不该遇到；真遇到了也只给通用文案。
    expect(describeStudentClassroomError(new ApiError({ code: 'CLASSROOM_NOT_OWNER' }))).toBe(
      API_ERROR_MESSAGES.CLASSROOM_NOT_OWNER,
    )
    expect(describeStudentClassroomError(new ApiError({ code: 'INTERNAL', status: 500 }))).toBe(
      API_ERROR_MESSAGES.INTERNAL,
    )
  })

  it('join 被拒的三种码有学生专用文案，且都指向下一步动作（§43）', () => {
    // 学生此刻已经共享着整块屏幕：含糊的"进入失败"会让他保持共享并反复点按钮。
    const closed = describeStudentClassroomError(
      new ApiError({ code: 'CLASSROOM_CLOSED', status: 409 }),
    )
    expect(closed).toContain('老师关闭')
    expect(closed).toContain('返回我的课堂')

    const duplicated = describeStudentClassroomError(
      new ApiError({ code: 'SESSION_ALREADY_ACTIVE', status: 409 }),
    )
    expect(duplicated).toContain('另一个页面')

    const mediaToken = describeStudentClassroomError(
      new ApiError({ code: 'MEDIA_TOKEN_FAILED', status: 502 }),
    )
    expect(mediaToken).toContain('请稍后重试')
  })

  it('非 ApiError 的意外失败也降级成通用文案，不把异常内容渲染给学生', () => {
    expect(describeStudentClassroomError(new Error('<html>boom</html>'))).toBe(
      API_ERROR_MESSAGES.INTERNAL,
    )
    expect(describeStudentClassroomError('500 Internal Server Error')).toBe(
      API_ERROR_MESSAGES.INTERNAL,
    )
    expect(describeStudentClassroomError(undefined)).toBe(API_ERROR_MESSAGES.INTERNAL)
  })

  it('无论哪种失败，文案里都不出现状态码或英文原始报文', () => {
    const failures: unknown[] = [
      new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 }),
      new ApiError({ code: 'INTERNAL', status: 500, message: 'Internal Server Error' }),
      new Error('TypeError: Failed to fetch'),
    ]

    for (const failure of failures) {
      const message = describeStudentClassroomError(failure)
      expect(message).not.toMatch(/[A-Za-z]{4,}/)
      expect(message).not.toMatch(/\b(4\d\d|5\d\d)\b/)
    }
  })

  it('只把 STUDENT_NOT_ASSIGNED 认成"不在名单里"（视图据此换成说明 + 回列表）', () => {
    expect(isNotAssigned(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 }))).toBe(true)
    // 课堂未开启、课堂不存在都不是这个码：它们的处理方式完全不同。
    expect(isNotAssigned(new ApiError({ code: 'CLASSROOM_CLOSED', status: 403 }))).toBe(false)
    expect(isNotAssigned(new ApiError({ code: 'CLASSROOM_NOT_FOUND', status: 404 }))).toBe(false)
    expect(isNotAssigned(new Error('boom'))).toBe(false)
  })
})
