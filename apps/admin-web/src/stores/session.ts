import type { User } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

/**
 * 管理端会话状态。
 *
 * Phase 0 只有内存状态，**没有任何网络调用**，也不做持久化。
 *
 * Phase 1 会在这里补两件事：
 * 1. 登录：POST /api/v1/admin/auth/login（account + password，§40），
 *    与教师端同样的密码认证，后端校验 password hash；
 * 2. 恢复会话：GET /api/v1/admin/auth/me，HttpOnly Cookie 由浏览器自动携带；
 *    失败（AUTH_REQUIRED / ACCOUNT_DISABLED）时 clear()，由路由守卫重定向到登录页。
 *
 * WHY 不把 user 或权限标记存 localStorage：
 * 管理端能创建账号、停用账号，是权限最高的入口。任何前端持久化的"我是 ADMIN"
 * 都只是自欺欺人：每个 /api/v1/admin/* 请求仍必须由后端按 role == ADMIN 独立校验（§37）。
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
