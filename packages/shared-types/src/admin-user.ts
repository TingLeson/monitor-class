import type { IsoDateTime, Uuid } from './common'
import type { Role, UserStatus } from './user'

/**
 * 管理端用户管理契约（§4 / §9 / §42 Admin / §68）。
 *
 * 端点（与后端冻结的契约一一对应）：
 *
 *   GET    /api/v1/admin/users                       列表（筛选 + 分页）
 *   POST   /api/v1/admin/users                       创建老师 / 学生
 *   GET    /api/v1/admin/users/:id                   查看单个账号
 *   PATCH  /api/v1/admin/users/:id                   编辑显示名（只允许 displayName）
 *   PATCH  /api/v1/admin/users/:id/status            启用 / 停用
 *   POST   /api/v1/admin/teachers/:id/reset-password 重置老师密码
 *
 * WHY 这些类型不放进 user.ts：`User` 是 §9 的用户表 DTO，`AuthUser` 是会话里的身份，
 * 三者字段集不同（这里没有 `createdBy`，但多了 `updatedAt`）。合成一个"万能 User"
 * 会让某个接口少返一个字段时前端毫无察觉——类型分开了，契约漂移才会在编译期暴露。
 */

/**
 * 管理员可以创建的角色（§4）。
 *
 * WHY 不是 `Role`：§4 只赋予管理员"创建老师 / 创建学生"的能力，第一个管理员由环境
 * 变量或初始化命令（`make create-admin`）创建。`POST /api/v1/admin/users` 对
 * `role: "ADMIN"` 直接返回 400——若管理端 API 能造管理员，一个被盗的管理员会话
 * 就能留下一个永久后门。这里用独立的联合类型，让"创建管理员"在类型层面也无法表达。
 */
export const ADMIN_CREATABLE_ROLES = ['TEACHER', 'STUDENT'] as const

export type AdminCreatableRole = (typeof ADMIN_CREATABLE_ROLES)[number]

/**
 * 管理端用户 DTO：列表与详情**同形**（后端冻结的契约）。
 *
 * 与 `User`（§9）的差异是刻意的：没有 `createdBy`（契约没有这个字段，界面也不展示
 * "谁创建的"），有 `updatedAt`（"改过显示名/停用时间"是管理员排障时要看的）。
 * 任何情况下都不含 `passwordHash`——后端 DTO 里根本没有这个字段（§9 / §63）。
 */
export interface AdminUser {
  id: Uuid
  account: string
  displayName: string
  role: Role
  status: UserStatus
  createdAt: IsoDateTime
  updatedAt: IsoDateTime
  /** 从未登录过为 null：这是"账号发出去了但没人用过"的唯一线索，界面必须友好显示。 */
  lastLoginAt: IsoDateTime | null
}

/**
 * 列表查询参数。
 *
 * 后端行为（冻结契约）：`role` / `status` / `q` 都可选；`q` 模糊匹配账号或显示名；
 * `page` 默认 1；`pageSize` 默认 50、上限 200。排序固定 `created_at DESC`（稳定排序）——
 * 前端不提供"任意列排序"，否则翻页会出现重复项或漏项。
 */
export interface AdminUserListQuery {
  role?: Role
  status?: UserStatus
  /** 账号 / 显示名模糊匹配。账号是 citext，因此天然大小写不敏感。 */
  q?: string
  page?: number
  pageSize?: number
}

export interface AdminUserListResponse {
  users: AdminUser[]
  total: number
  page: number
  pageSize: number
}

/**
 * 创建账号请求（§2.2 / §4 / §9）。
 *
 * WHY 用可辨识联合而不是"role + 可选 password"：
 * - `TEACHER` 必须有初始密码（后端与数据库 `users_password_by_role` 都强制）；
 * - `STUDENT` **必须没有** password（§2.2 学生免密是业务规则；带 password 会被后端
 *   400 拒绝）。两个分支让"带密码的学生"在类型层面就写不出来，而不是靠提交时才报错。
 *
 * 账号格式由数据库约束 `users_account_format` 固定为 `^[A-Za-z0-9._-]{3,64}$`：
 * 学生账号是唯一凭据，格式本身是安全边界（禁止空白与同形字符）。
 */
export type CreateUserRequest =
  | { account: string; displayName: string; role: 'TEACHER'; password: string }
  | { account: string; displayName: string; role: 'STUDENT' }

/**
 * 编辑显示名请求。
 *
 * 只允许 `displayName`：§4 的管理员能力清单里没有"修改角色"，改角色会牵动会话角色、
 * 课堂归属（`classrooms.owner_teacher_id` 要求 owner 是 TEACHER）与历史 Run 归属。
 * 请求体里出现 `role` / `status` / `password` 时后端返回 400 而不是静默忽略——
 * 静默忽略会让前端以为改成功了，是最难排查的一类 Bug。
 */
export interface UpdateDisplayNameRequest {
  displayName: string
}

/** 启用 / 停用请求。停用会立即撤销该账号全部会话（§41）。 */
export interface UpdateUserStatusRequest {
  status: UserStatus
}

/**
 * 重置老师密码请求（§4）。
 *
 * 不传 `password` 时由服务端用 crypto/rand 生成高熵密码，并**只在这一次响应里**返回。
 * 为什么提供生成模式：现实中"管理员随便想一个密码"往往就是 `123456` 或老师的生日，
 * 让服务端生成能把"弱密码"这条风险从流程里去掉。
 */
export interface ResetTeacherPasswordRequest {
  password?: string
}

/** 重置老师密码响应；`password` 仅在服务端生成时出现，且只出现一次。 */
export interface ResetTeacherPasswordResponse {
  user: AdminUser
  password?: string
}

/**
 * 老师 / 管理员密码的最小长度。
 *
 * 真相在服务端：`PASSWORD_MIN_LENGTH` 可配置（默认 12，下限 8，见
 * services/api/internal/auth/password.go）。前端常量只是为了让用户在提交**之前**
 * 就知道规则，避免白跑一次请求；真正的判定永远是后端的 PASSWORD_POLICY_VIOLATION，
 * 且该错误码会被挂到密码输入框上（后端 message 里带具体规则）。
 */
export const PASSWORD_MIN_LENGTH = 12

/** 列表分页契约：后端默认 50、上限 200。前端只用它做默认值与请求前的钳制。 */
export const ADMIN_USER_PAGE_SIZE_DEFAULT = 50
export const ADMIN_USER_PAGE_SIZE_MAX = 200
