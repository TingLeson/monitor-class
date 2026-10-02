import type { AuthUser, Role } from '@classwatch/shared-types'

/**
 * 测试夹具（三个 app 各一份）。
 *
 * WHY 不放进 @classwatch/shared-types 或 @classwatch/ui：那不是生产代码该有的导出，
 * 而且测试夹具一旦共享，某个 app 为图方便改一个默认值，另外两个 app 的测试就会
 * 在毫无关系的地方变红（§5 的入口隔离同样适用于测试）。
 */
export function makeAuthUser(role: Role, overrides: Partial<AuthUser> = {}): AuthUser {
  return {
    id: `user-${role.toLowerCase()}-1`,
    account: role === 'STUDENT' ? 'S10086' : role === 'TEACHER' ? 'teacher001' : 'admin',
    displayName: role === 'STUDENT' ? '张三' : role === 'TEACHER' ? '李老师' : '系统管理员',
    role,
    status: 'ACTIVE',
    createdAt: '2026-01-05T08:00:00Z',
    lastLoginAt: null,
    ...overrides,
  }
}

/** 构造带 error 字段的响应体（后端统一错误结构，§58）。 */
export function errorBody(code: string, message = '', requestId = 'rid-test') {
  return { error: { code, message }, requestId }
}
