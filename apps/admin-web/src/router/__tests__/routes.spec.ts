import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'
import { routes } from '../index'

/**
 * §55 的页面路径是硬契约：nginx location、后端 redirect 白名单、CORS allowlist
 * 都按这些字符串写死。这里逐条钉住，防止后续 Phase 重构时被"顺手改名"。
 */
const ADMIN_PATHS = [
  '/admin/login',
  '/admin/dashboard',
  '/admin/users',
  '/admin/users/new',
] as const

describe('管理端路由表（§55 Admin）', () => {
  it.each(ADMIN_PATHS)('声明了 %s', (path) => {
    expect(routes.map((route) => route.path)).toContain(path)
  })

  it('根路径重定向到管理员登录页', () => {
    const root = routes.find((route) => route.path === '/')
    expect(root?.redirect).toBe('/admin/login')
  })

  it('/admin/users/new 命中新建页，/admin/users 命中列表页（互不串位）', async () => {
    const router = createRouter({ history: createMemoryHistory(), routes })

    await router.push('/admin/users/new')
    expect(router.currentRoute.value.name).toBe('admin-user-new')

    await router.push('/admin/users')
    expect(router.currentRoute.value.name).toBe('admin-users')
  })

  it('不包含学生端 / 教师端路径：三个入口是物理分离的 SPA（§5）', () => {
    expect(routes.some((route) => route.path.startsWith('/student'))).toBe(false)
    expect(routes.some((route) => route.path.startsWith('/teacher'))).toBe(false)
  })
})
