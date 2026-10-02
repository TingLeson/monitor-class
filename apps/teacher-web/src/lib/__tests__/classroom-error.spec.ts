import { ApiError } from '@classwatch/api-client'
import { API_ERROR_MESSAGES } from '@classwatch/shared-types'
import { describe, expect, it } from 'vitest'
import {
  classroomConflictNotice,
  classroomErrorField,
  describeClassroomError,
  isClassroomStateConflict,
  studentRejectionReason,
} from '../classroom-error'

/**
 * 错误码 → 界面行为（§58；docs/frontend/teacher.md §6）。
 *
 * 这一层错了会以"界面说了假话"的形式出现：把冲突说成失败、把英文后台文案渲染在
 * 中文界面里、把后端新错误码显示成空白。因此每条分支都要有断言。
 */
describe('describeClassroomError', () => {
  it('已知错误码走共享中文文案（§58 的 API_ERROR_MESSAGES）', () => {
    const error = new ApiError({ code: 'CLASSROOM_NOT_OWNER', status: 403 })

    expect(describeClassroomError(error)).toBe(API_ERROR_MESSAGES.CLASSROOM_NOT_OWNER)
  })

  it('非 ApiError（代码 bug、本地异常）降级为通用内部错误，而不是把原始异常文本显示出来', () => {
    expect(describeClassroomError(new Error('boom'))).toBe(API_ERROR_MESSAGES.INTERNAL)
    expect(describeClassroomError(undefined)).toBe(API_ERROR_MESSAGES.INTERNAL)
  })

  it('后端给了已本地化的具体说明时采用它（INVALID_REQUEST 的后端 message 更有用）', () => {
    const error = new ApiError({
      code: 'INVALID_REQUEST',
      message: '课堂名称不能超过 80 个字符',
      status: 400,
    })

    expect(describeClassroomError(error)).toBe('课堂名称不能超过 80 个字符')
  })

  it('后端 message 是英文时退回中文兜底文案（不把英文句子渲染进中文界面）', () => {
    const error = new ApiError({
      code: 'INVALID_REQUEST',
      message: 'name must be 1..80 characters',
      status: 400,
    })

    expect(describeClassroomError(error)).toBe(API_ERROR_MESSAGES.INVALID_REQUEST)
  })

  it('后端 message 与默认文案相同时不重复取用', () => {
    const error = new ApiError({
      code: 'STUDENT_NOT_FOUND',
      message: API_ERROR_MESSAGES.STUDENT_NOT_FOUND,
      status: 404,
    })

    expect(describeClassroomError(error)).toBe(API_ERROR_MESSAGES.STUDENT_NOT_FOUND)
  })
})

describe('状态冲突（§7 / §48 / §49）', () => {
  it('只有 ALREADY_OPEN / ALREADY_CLOSED 算状态冲突', () => {
    expect(isClassroomStateConflict('CLASSROOM_ALREADY_OPEN')).toBe(true)
    expect(isClassroomStateConflict('CLASSROOM_ALREADY_CLOSED')).toBe(true)
    expect(isClassroomStateConflict('CLASSROOM_NOT_FOUND')).toBe(false)
    expect(isClassroomStateConflict('NETWORK_ERROR')).toBe(false)
  })

  it('冲突提示必须同时说明"真实状态"与"列表已刷新"', () => {
    expect(classroomConflictNotice('CLASSROOM_ALREADY_OPEN')).toBe(
      '课堂已经是开启状态，列表已刷新。',
    )
    expect(classroomConflictNotice('CLASSROOM_ALREADY_CLOSED')).toBe(
      '课堂已经是关闭状态，列表已刷新。',
    )
  })

  it('非冲突码没有冲突提示（避免把普通失败说成"状态已刷新"）', () => {
    expect(classroomConflictNotice('CLASSROOM_NOT_FOUND')).toBeNull()
    expect(classroomConflictNotice('INVALID_REQUEST')).toBeNull()
  })
})

describe('批量添加的逐条拒绝原因（§11）', () => {
  it.each([
    ['STUDENT_NOT_FOUND', '账号不存在，请核对账号'],
    ['NOT_A_STUDENT', '该账号不是学生账号'],
    ['ACCOUNT_DISABLED', '账号已被停用，请联系管理员启用'],
    ['INVALID_REQUEST', '账号格式不合法'],
  ] as const)('%s 给出可执行的中文原因', (code, expected) => {
    expect(studentRejectionReason(code)).toBe(expected)
  })

  it('未登记的拒绝码回退到共享文案，而不是编造原因', () => {
    expect(studentRejectionReason('SESSION_NOT_FOUND')).toBe(API_ERROR_MESSAGES.SESSION_NOT_FOUND)
  })
})

describe('错误码 → 表单字段', () => {
  it('整批拒绝的账号类错误挂到账号输入框', () => {
    expect(classroomErrorField('STUDENT_NOT_FOUND')).toBe('accounts')
    expect(classroomErrorField('NOT_A_STUDENT')).toBe('accounts')
  })

  it('INVALID_REQUEST 不猜字段：它同时覆盖名称/说明/未知字段，一律走页面级提示', () => {
    expect(classroomErrorField('INVALID_REQUEST')).toBeNull()
    expect(classroomErrorField('CLASSROOM_ALREADY_OPEN')).toBeNull()
    expect(classroomErrorField('NETWORK_ERROR')).toBeNull()
  })
})
