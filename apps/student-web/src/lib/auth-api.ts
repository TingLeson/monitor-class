import type { AuthMeResponse, AuthUser, LoginResponse } from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 学生端认证接口（§38 / §41 / §67）。
 *
 * 路径与 Cookie 名都取自后端冻结的契约：
 * - POST /api/v1/student/auth/login  body 只有 account（学生无密码，§2.2）
 * - GET  /api/v1/student/auth/me
 * - POST /api/v1/student/auth/logout -> 204
 */
const BASE_PATH = '/api/v1/student/auth'

/**
 * 登录：请求体里只有 account。
 *
 * WHY 不加密码/验证码字段：§2.2 明确列出 ❌ 密码、❌ 验证码、❌ 邮箱验证、
 * ❌ 注册页面。学生无密码是已接受的业务模型，前端擅自加字段等于改需求。
 */
export async function login(account: string): Promise<AuthUser> {
  const response = await api.post<LoginResponse>(`${BASE_PATH}/login`, { account })
  return response.user
}

/**
 * 读取当前会话对应的用户——刷新页面后唯一可信的身份来源。
 *
 * 调用方必须按错误码分支（见 session store）：
 * - 401 AUTH_REQUIRED：没有会话/已过期/已撤销，属于**正常路径**，不是异常；
 * - 403 ACCOUNT_DISABLED：账号被管理员停用；
 * - 403 ROLE_FORBIDDEN：带着别的入口的会话访问本入口（§67 要求跨入口拒绝）；
 * - NETWORK_ERROR：请求根本没到服务器，**不得**推断为"未登录"。
 */
export async function fetchCurrentUser(): Promise<AuthUser> {
  const response = await api.get<AuthMeResponse>(`${BASE_PATH}/me`)
  return response.user
}

/** 登出：后端撤销会话并清除两个 Cookie（204，无响应体）。 */
export async function logout(): Promise<void> {
  await api.post<void>(`${BASE_PATH}/logout`)
}
