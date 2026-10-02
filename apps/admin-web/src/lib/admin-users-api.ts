import type {
  AdminUser,
  AdminUserListQuery,
  AdminUserListResponse,
  CreateUserRequest,
  ResetTeacherPasswordRequest,
  ResetTeacherPasswordResponse,
  UpdateDisplayNameRequest,
  UpdateUserStatusRequest,
} from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 管理端用户管理接口（§42 Admin / §68）。
 *
 * 复用本 app 的 api client（./api.ts）：Cookie 会话、写请求自动附加 X-CSRF-Token、
 * 统一错误体 → ApiError 都由它保证。这里只负责"路径 + 请求体 + 响应体形状"，
 * 不做任何业务判断——权限与状态规则的唯一执行点是后端（§37 / §63）。
 */
const BASE_PATH = '/api/v1/admin/users'
const TEACHERS_PATH = '/api/v1/admin/teachers'

/**
 * 列表（筛选 + 分页）。
 *
 * `signal` 透传给 fetch：页面卸载或用户改筛选时可以取消上一个请求。
 * 排序由后端固定为 `created_at DESC`（稳定排序），前端不提供任意列排序。
 */
export function listUsers(
  query: AdminUserListQuery = {},
  options: { signal?: AbortSignal } = {},
): Promise<AdminUserListResponse> {
  return api.get<AdminUserListResponse>(BASE_PATH, {
    query: {
      role: query.role,
      status: query.status,
      q: query.q,
      page: query.page,
      pageSize: query.pageSize,
    },
    signal: options.signal,
  })
}

/**
 * 从成功响应里取出用户 DTO。
 *
 * 后端（services/api/internal/httpapi/admin.go）目前对创建 / 详情 / 改名 / 改状态
 * 统一返回 `{ "user": {...} }`；而冻结的契约文字只写了"返回用户 DTO"。
 * 两种形状都接受，是为了让信封这种与界面无关的细节不至于把一次**已经成功**的写操作
 * 变成一个错误提示。这里只统一成功响应体形状：非 2xx 依旧由 api-client 抛 ApiError，不吞错。
 */
function readUserPayload(payload: AdminUser | { user: AdminUser }): AdminUser {
  return 'user' in payload ? payload.user : payload
}

/** 查看单个账号（列表与详情同形）。 */
export async function getUser(id: string): Promise<AdminUser> {
  const payload = await api.get<AdminUser | { user: AdminUser }>(`${BASE_PATH}/${id}`)
  return readUserPayload(payload)
}

/**
 * 创建老师 / 学生（201 + Location 头 + 用户 DTO）。role=ADMIN 由后端拒绝（400）。
 *
 * 用可辨识联合 `CreateUserRequest` 而不是"role + 可选密码"：学生分支在类型上就没有
 * password 字段，杜绝了"带密码的学生"（§2.2 / §9）。
 */
export async function createUser(input: CreateUserRequest): Promise<AdminUser> {
  const payload = await api.post<AdminUser | { user: AdminUser }>(BASE_PATH, input)
  return readUserPayload(payload)
}

/**
 * 编辑显示名（只允许 displayName；传 role/status/password 会被后端 400 拒绝）。
 *
 * 返回 `AdminUser | null`：后端目前是 200 + `{ user }`，但契约没有写死 PATCH 一定带响应体
 * （204 也是合法实现）。拿不到 DTO 时返回 null，由 store 重新拉列表，
 * 而不是让调用方拿到一个只在本地拼出来的"半成品用户"。
 */
export async function updateDisplayName(
  id: string,
  input: UpdateDisplayNameRequest,
): Promise<AdminUser | null> {
  const payload = await api.patch<AdminUser | { user: AdminUser } | undefined>(
    `${BASE_PATH}/${id}`,
    input,
  )
  return payload ? readUserPayload(payload) : null
}

/**
 * 启用 / 停用。停用会立即撤销该账号的全部会话（§41），因此调用方必须重新拉列表，
 * 而不是本地改一个字段假装成功。
 */
export async function updateUserStatus(
  id: string,
  input: UpdateUserStatusRequest,
): Promise<AdminUser | null> {
  const payload = await api.patch<AdminUser | { user: AdminUser } | undefined>(
    `${BASE_PATH}/${id}/status`,
    input,
  )
  return payload ? readUserPayload(payload) : null
}

/**
 * 重置老师密码（§4）。不传 password 时服务端生成，并**只在这一次响应里**返回明文。
 *
 * 返回值直接交给调用方展示一次，绝不再往上传（不进 store 状态、不落 Web Storage）。
 */
export function resetTeacherPassword(
  id: string,
  input: ResetTeacherPasswordRequest = {},
): Promise<ResetTeacherPasswordResponse> {
  return api.post<ResetTeacherPasswordResponse>(`${TEACHERS_PATH}/${id}/reset-password`, input)
}
