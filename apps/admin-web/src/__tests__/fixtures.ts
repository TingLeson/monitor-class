import type { AdminUser, AdminUserListResponse, AuthUser, Role } from '@classwatch/shared-types'

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

/**
 * 管理端用户 DTO 夹具。默认是一个"刚创建、从未登录"的管理员账号：
 * 测试只需要覆盖自己在意的字段，其余保持稳定，避免断言被无关字段的默认值影响。
 */
export function makeAdminUser(overrides: Partial<AdminUser> = {}): AdminUser {
  return {
    id: 'user-admin-1',
    account: 'admin',
    displayName: '系统管理员',
    role: 'ADMIN',
    status: 'ACTIVE',
    createdAt: '2026-01-05T08:00:00Z',
    updatedAt: '2026-01-05T08:00:00Z',
    lastLoginAt: null,
    ...overrides,
  }
}

/** 列表响应；total 默认等于传入的条数，需要"总数大于当前页"时用 overrides 指定。 */
export function makeUserListResponse(
  users: AdminUser[],
  overrides: Partial<AdminUserListResponse> = {},
): AdminUserListResponse {
  return { users, total: users.length, page: 1, pageSize: 50, ...overrides }
}

/** 常用角色的用户夹具，让用例读起来像业务句子。 */
export function makeTeacher(overrides: Partial<AdminUser> = {}): AdminUser {
  return makeAdminUser({
    id: 'user-teacher-1',
    account: 'teacher001',
    displayName: '李老师',
    role: 'TEACHER',
    ...overrides,
  })
}

export function makeStudent(overrides: Partial<AdminUser> = {}): AdminUser {
  return makeAdminUser({
    id: 'user-student-1',
    account: 'S10086',
    displayName: '张三',
    role: 'STUDENT',
    ...overrides,
  })
}

/** 构造带 error 字段的响应体（后端统一错误结构，§58）。 */
export function errorBody(code: string, message = '', requestId = 'rid-test') {
  return { error: { code, message }, requestId }
}
