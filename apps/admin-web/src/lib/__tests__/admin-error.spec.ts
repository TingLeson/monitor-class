import { ApiError } from '@classwatch/api-client'
import { describe, expect, it } from 'vitest'
import { createUserErrorField, describeAdminError } from '../admin-error'

/**
 * 错误码 → 用户可见文案 / 表单字段（§58；docs/frontend/admin.md §7）。
 *
 * 这一层的价值全在"什么都不该显示"的那半边：未知错误码、后端新增的码、
 * 甚至 500 的 HTML，都必须在到达界面前被折叠成一句可读的中文。
 */
describe('describeAdminError', () => {
  it.each([
    ['AUTH_REQUIRED', '登录状态已失效'],
    ['ROLE_FORBIDDEN', '无权'],
    ['RATE_LIMITED', '稍后再试'],
    ['CSRF_INVALID', '刷新页面'],
    ['ACCOUNT_ALREADY_EXISTS', '已存在'],
    ['USER_NOT_FOUND', '不存在'],
    ['PASSWORD_POLICY_VIOLATION', '密码'],
    ['INVALID_REQUEST', '请求未被接受'],
    ['NETWORK_ERROR', '网络连接失败'],
  ] as const)('%s → 中文文案（含「%s」）', (code, expected) => {
    expect(describeAdminError(new ApiError({ code }))).toContain(expected)
  })

  it('后端给了中文的具体原因时优先展示（比通用文案更有用）', () => {
    const error = new ApiError({
      code: 'INVALID_REQUEST',
      status: 400,
      message: '不能停用最后一个管理员，请先创建另一个管理员',
    })

    expect(describeAdminError(error)).toBe('不能停用最后一个管理员，请先创建另一个管理员')
  })

  it('后端 message 还是英文时回退到中文文案（中文界面里不出现整句英文）', () => {
    const error = new ApiError({
      code: 'INVALID_REQUEST',
      status: 400,
      message:
        'the last active administrator cannot be disabled; create another administrator first',
    })

    const message = describeAdminError(error)

    expect(message).toBe('请求未被接受，请检查填写内容后重试。')
    expect(message).not.toMatch(/[a-z]{4}/)
  })

  it('未知错误码（api-client 降级为 INTERNAL）只给通用文案，绝不透出原始报文', () => {
    const error = new ApiError({
      code: 'INTERNAL',
      status: 500,
      message: '<html><body>500 Internal Server Error</body></html>',
    })

    const message = describeAdminError(error)

    expect(message).toBe('服务器内部错误，请稍后重试。')
    expect(message).not.toContain('html')
    expect(message).not.toContain('Internal Server Error')
  })

  it('不是 ApiError 的异常也折叠成通用文案', () => {
    expect(describeAdminError(new TypeError('Failed to fetch'))).toBe(
      '服务器内部错误，请稍后重试。',
    )
    expect(describeAdminError(undefined)).toBe('服务器内部错误，请稍后重试。')
  })
})

describe('createUserErrorField', () => {
  it('账号重复挂到账号字段，密码策略挂到密码字段', () => {
    expect(createUserErrorField('ACCOUNT_ALREADY_EXISTS')).toBe('account')
    expect(createUserErrorField('PASSWORD_POLICY_VIOLATION')).toBe('password')
  })

  it('其余错误没有对应字段（走顶部提示，而不是硬塞进某个输入框）', () => {
    for (const code of ['INVALID_REQUEST', 'RATE_LIMITED', 'INTERNAL', 'NETWORK_ERROR'] as const) {
      expect(createUserErrorField(code)).toBeNull()
    }
  })
})
