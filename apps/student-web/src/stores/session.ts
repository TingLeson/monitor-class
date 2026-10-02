import type { User } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

/**
 * 学生端会话状态。
 *
 * Phase 0 只有内存状态，**没有任何网络调用**，也不做持久化。
 *
 * Phase 1 会在这里补两件事（这是本文件的唯一演进方向）：
 * 1. 登录：POST /api/v1/student/auth/login（body 只有 account，§38），
 *    成功后 setUser(响应里的 User)；
 * 2. 恢复会话：GET /api/v1/student/auth/me，HttpOnly Cookie 由浏览器自动携带；
 *    失败（AUTH_REQUIRED / ACCOUNT_DISABLED）时 clear()，由路由守卫重定向到登录页。
 *
 * WHY 不把 user 存 localStorage：
 * - 会话凭证必须只存在于 HttpOnly Cookie（§38/§41），localStorage 里的 user 只是副本，
 *   刷新页面仍必须用 /auth/me 重新确认，否则被停用/已登出的账号还会看到已登录界面；
 * - 一旦允许持久化 user，后续 Phase 很容易顺手把 token 也放进去，直接违背 §38。
 */
export const useSessionStore = defineStore('session', () => {
  const user = ref<User | null>(null)

  /**
   * 登录态由"后端返回的 user"唯一决定。
   * WHY 用 computed 而不是独立的布尔字段：两个字段一旦不同步（setUser 忘记改布尔值），
   * 就会出现"界面显示已登录但请求全部 401"的诡异状态。
   */
  const isAuthenticated = computed(() => user.value !== null)

  function setUser(nextUser: User): void {
    user.value = nextUser
  }

  function clear(): void {
    user.value = null
  }

  return { user, isAuthenticated, setUser, clear }
})
