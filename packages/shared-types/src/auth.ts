import type { IsoDateTime, Uuid } from './common'
import type { Role, UserStatus } from './user'

/**
 * 登录会话契约（§37 / §38 / §39 / §40 / §41 / §67）。
 *
 * 三个入口的认证接口形状刻意保持一致，只有请求体不同（学生无密码）：
 *
 *   POST /api/v1/student/auth/login  { account }                     -> { user }
 *   POST /api/v1/teacher/auth/login  { account, password }            -> { user }
 *   POST /api/v1/admin/auth/login    { account, password }            -> { user }
 *   GET  /api/v1/{student|teacher|admin}/auth/me                     -> { user }
 *   POST /api/v1/{student|teacher|admin}/auth/logout                 -> 204
 *
 * 会话本身**不在**响应体里：后端只下发 HttpOnly Cookie（§38 / §41），
 * 前端拿不到也不需要拿 session token。
 */

/**
 * 认证接口返回的用户 DTO。
 *
 * WHY 不复用 `User`（§9 的 users 表 DTO）：
 * 1. 前端登录只需要"我是谁、什么角色、能不能用"，`createdBy` / `updatedAt`
 *    属于管理端（Phase 2 的 /admin/users）才需要的字段，认证接口不必返回；
 * 2. 让认证响应尽量小，避免把账号管理字段顺手带到每个入口的会话里；
 * 3. 类型上把两者分开后，"会话里的用户"与"用户管理列表里的用户"不会互相污染。
 *
 * 约束：
 * - `status` 必须由后端在登录与 /auth/me 时都复核（§37）：DISABLED 账号不得登录，
 *   已被停用的账号其既有会话必须表现为 ACCOUNT_DISABLED 而不是继续可用；
 * - 前端**只**允许把这些字段当展示信息：任何权限判断（能否开课、能否进课堂）
 *   都必须由后端按 role + 资源归属独立裁决（§37 / §63）。
 */
export interface AuthUser {
  id: Uuid
  account: string
  displayName: string
  role: Role
  status: UserStatus
  createdAt: IsoDateTime
  /** 本次登录之前的最后一次登录时间；从未登录过为 null。 */
  lastLoginAt: IsoDateTime | null
}

/**
 * 学生登录请求（§38）。
 *
 * WHY 只有 account：学生账号无密码是**明确的业务规则**（§2.2：不设密码、不注册、
 * 无验证码/邮箱/手机号验证）。这不是"简化实现"，也不是残缺的强身份认证；
 * 因此前端不得擅自加密码框、注册链接或"忘记密码"，那反而违背已冻结的需求。
 */
export interface StudentLoginRequest {
  account: string
}

/**
 * 教师/管理员登录请求（§39 / §40）。后端校验 password hash（§63）。
 *
 * 密码只在这一个请求体里出现：不进 Pinia、不进 localStorage/sessionStorage、
 * 不写日志、不回显（§38 / §41 / §59）。
 */
export interface PasswordLoginRequest {
  account: string
  password: string
}

/** 登录成功响应（三个入口共用）。`user` 是唯一可信的"当前身份"来源。 */
export interface LoginResponse {
  user: AuthUser
}

/**
 * /auth/me 的响应与登录响应同形；单独起名是为了让调用点自解释
 * （读会话 ≠ 刚登录），也便于后端将来只改其中一个时类型能体现差异。
 */
export interface AuthMeResponse {
  user: AuthUser
}

/** 会话状态机（前端内存态）。三态而不是布尔值，见 session store 的注释。 */
export const SESSION_STATUSES = ['unknown', 'anonymous', 'authenticated'] as const

export type SessionStatus = (typeof SESSION_STATUSES)[number]

export function isAuthUser(value: unknown): value is AuthUser {
  if (typeof value !== 'object' || value === null) return false
  const candidate = value as Record<string, unknown>
  return (
    typeof candidate.id === 'string' &&
    typeof candidate.account === 'string' &&
    typeof candidate.displayName === 'string' &&
    typeof candidate.role === 'string' &&
    typeof candidate.status === 'string' &&
    typeof candidate.createdAt === 'string' &&
    (candidate.lastLoginAt === null || typeof candidate.lastLoginAt === 'string')
  )
}
