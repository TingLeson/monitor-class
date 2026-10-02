import type { AuthMeResponse, AuthUser, LoginResponse } from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 管理端认证接口（§40 / §41 / §67）。
 *
 * 路径与 Cookie 名都取自后端冻结的契约：
 * - POST /api/v1/admin/auth/login  body 是 account + password
 * - GET  /api/v1/admin/auth/me
 * - POST /api/v1/admin/auth/logout -> 204
 */
const BASE_PATH = '/api/v1/admin/auth'

/**
 * 登录（§40）：后端校验 password hash。
 *
 * 密码只出现在这一个请求体里：不进 Pinia、不进 localStorage/sessionStorage、
 * 不写日志、不回显（§41 / §59）。
 */
export async function login(account: string, password: string): Promise<AuthUser> {
  const response = await api.post<LoginResponse>(`${BASE_PATH}/login`, { account, password })
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
