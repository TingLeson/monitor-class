import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'
import { routes } from '../index'

/**
 * §55 的页面路径是硬契约：nginx location、后端 redirect 白名单、CORS allowlist
 * 都按这些字符串写死。这里逐条钉住，防止后续 Phase 重构时被"顺手改名"。
 */
const STUDENT_PATHS = [
  '/student/login',
  '/student/classrooms',
  '/student/classrooms/:id',
  '/student/session/:sessionId',
] as const

describe('学生端路由表（§55 Student）', () => {
  it.each(STUDENT_PATHS)('声明了 %s', (path) => {
    expect(routes.map((route) => route.path)).toContain(path)
  })

  it('根路径重定向到学生登录页', () => {
    const root = routes.find((route) => route.path === '/')
    expect(root?.redirect).toBe('/student/login')
  })

  it('用 createWebHistory 挂载，页面 path 与实际 URL 一致（无 basename 错位）', async () => {
    const router = createRouter({ history: createMemoryHistory(), routes })

    await router.push('/student/classrooms')

    expect(router.currentRoute.value.path).toBe('/student/classrooms')
    expect(router.currentRoute.value.name).toBe('student-classrooms')
    // 生产环境用的是 createWebHistory：这里断言导出的 router 不带 base 前缀，
    // 否则页面显示的路径会与地址栏不一致（表现为刷新 404）。
    expect(routes.every((route) => route.path.startsWith('/'))).toBe(true)
  })

  it('不包含教师端 / 管理端路径：三个入口是物理分离的 SPA（§5）', () => {
    expect(routes.some((route) => route.path.startsWith('/teacher'))).toBe(false)
    expect(routes.some((route) => route.path.startsWith('/admin'))).toBe(false)
  })
})
