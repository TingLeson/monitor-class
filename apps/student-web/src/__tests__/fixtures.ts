import type { AuthUser, Role, StudentClassroom } from '@classwatch/shared-types'

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
 * 学生视角的课堂夹具（§14）。
 *
 * 默认是"已建好、老师还没开课、没有任何 currentRun"的课堂——这个状态最不容易
 * 掩盖 bug：任何"看起来可以进入"的断言都必须显式覆盖 status。
 *
 * 注意夹具里**没有** studentCount 之类的字段，也不允许加：一旦测试夹具能造出
 * "班级人数"，就一定会有人把它渲染出来，而 §26 明确禁止展示这类信息。
 * 夹具的形状本身就是一道防线。
 */
export function makeStudentClassroom(overrides: Partial<StudentClassroom> = {}): StudentClassroom {
  return {
    id: 'room-1',
    name: 'C++ 算法训练',
    description: '第三章 动态规划',
    status: 'CLOSED',
    teacher: { displayName: '王老师' },
    currentRun: null,
    createdAt: '2026-02-01T08:00:00Z',
    ...overrides,
  }
}

/** 已开启的课堂：列表页的「进入课堂」与「本次开始于」用的就是它（§14）。 */
export function makeOpenStudentClassroom(
  overrides: Partial<StudentClassroom> = {},
): StudentClassroom {
  return makeStudentClassroom({
    id: 'room-open-1',
    name: '数据结构练习',
    status: 'OPEN',
    teacher: { displayName: '李老师' },
    currentRun: { id: 'run-1', openedAt: '2026-02-03T06:05:00Z' },
    ...overrides,
  })
}

/** 构造带 error 字段的响应体（后端统一错误结构，§58）。 */
export function errorBody(code: string, message = '', requestId = 'rid-test') {
  return { error: { code, message }, requestId }
}
