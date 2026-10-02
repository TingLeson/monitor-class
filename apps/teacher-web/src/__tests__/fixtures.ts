import type {
  AuthUser,
  Classroom,
  ClassroomRun,
  ClassroomStudent,
  Role,
} from '@classwatch/shared-types'

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
 * 课堂 DTO 夹具。默认是一个"刚建好、从未开启、名单为空"的课堂——
 * 这个状态最不容易掩盖 bug：任何"看起来已经开课"的断言都必须显式覆盖 status。
 */
export function makeClassroom(overrides: Partial<Classroom> = {}): Classroom {
  return {
    id: 'room-1',
    name: 'C++ 算法训练',
    description: '第三章 动态规划',
    status: 'CLOSED',
    ownerTeacherId: 'user-teacher-1',
    studentCount: 0,
    currentRun: null,
    createdAt: '2026-02-01T08:00:00Z',
    updatedAt: '2026-02-01T08:00:00Z',
    ...overrides,
  }
}

/** 进行中的课堂（OPEN 且带 currentRun），列表页的"本次开始于"用的就是它。 */
export function makeOpenClassroom(overrides: Partial<Classroom> = {}): Classroom {
  return makeClassroom({
    id: 'room-open-1',
    name: '数据结构练习',
    status: 'OPEN',
    studentCount: 3,
    currentRun: { id: 'run-1', openedAt: '2026-02-03T06:05:00Z' },
    ...overrides,
  })
}

/** open / close 返回的完整 Run（§8）。 */
export function makeClassroomRun(overrides: Partial<ClassroomRun> = {}): ClassroomRun {
  return {
    id: 'run-1',
    classroomId: 'room-1',
    status: 'OPEN',
    openedAt: '2026-02-03T06:05:00Z',
    closedAt: null,
    ...overrides,
  }
}

/** 课堂名单里的一行（§11）。 */
export function makeClassroomStudent(overrides: Partial<ClassroomStudent> = {}): ClassroomStudent {
  return {
    id: 'user-student-1',
    account: 'S10086',
    displayName: '张三',
    status: 'ACTIVE',
    addedAt: '2026-02-02T09:30:00Z',
    ...overrides,
  }
}

/** 构造带 error 字段的响应体（后端统一错误结构，§58）。 */
export function errorBody(code: string, message = '', requestId = 'rid-test') {
  return { error: { code, message }, requestId }
}
