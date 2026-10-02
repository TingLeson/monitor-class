import type { IsoDateTime, Uuid } from './common'

/** 三类角色，与三个物理分离的 Web 入口一一对应（§5 / §2.1）。 */
export const ROLES = ['ADMIN', 'TEACHER', 'STUDENT'] as const

export type Role = (typeof ROLES)[number]

/** 账号状态（§9）。只有 ACTIVE 的账号能通过后端 Session Middleware（§37）。 */
export const USER_STATUSES = ['ACTIVE', 'DISABLED'] as const

export type UserStatus = (typeof USER_STATUSES)[number]

/**
 * 用户 DTO（§9 users 表）。
 *
 * WHY 没有 passwordHash 字段：Teacher/Admin 的密码哈希只存在于后端，
 * 任何接口都不得返回它。前端类型层面直接不给这个字段，避免有人"顺手"渲染或转发。
 *
 * 账号约束（§9）由后端保证，前端仅在表单校验时提示：
 * - STUDENT：无密码（password_hash MUST be NULL），学生登录只有 account；
 * - TEACHER / ADMIN：必须有密码。
 */
export interface User {
  id: Uuid
  account: string
  displayName: string
  role: Role
  status: UserStatus
  /** 创建该账号的管理员（学生/教师账号由 Admin 建立，§68）。 */
  createdBy: Uuid | null
  createdAt: IsoDateTime
  updatedAt: IsoDateTime
  lastLoginAt: IsoDateTime | null
}

export function isRole(value: unknown): value is Role {
  return typeof value === 'string' && (ROLES as readonly string[]).includes(value)
}

export function isUserStatus(value: unknown): value is UserStatus {
  return typeof value === 'string' && (USER_STATUSES as readonly string[]).includes(value)
}
