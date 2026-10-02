import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'
import { routes } from '../index'

/**
 * §55 的页面路径是硬契约：nginx location、后端 redirect 白名单、CORS allowlist
 * 都按这些字符串写死。这里逐条钉住，防止后续 Phase 重构时被"顺手改名"。
 */
const TEACHER_PATHS = [
  '/teacher/login',
  '/teacher/dashboard',
  '/teacher/classrooms',
  '/teacher/classrooms/new',
  '/teacher/classrooms/:id',
  '/teacher/classrooms/:id/monitor',
] as const

function createTestRouter() {
  return createRouter({ history: createMemoryHistory(), routes })
}

describe('教师端路由表（§55 Teacher）', () => {
  it.each(TEACHER_PATHS)('声明了 %s', (path) => {
    expect(routes.map((route) => route.path)).toContain(path)
  })

  it('根路径重定向到教师登录页', () => {
    const root = routes.find((route) => route.path === '/')
    expect(root?.redirect).toBe('/teacher/login')
  })

  it('/teacher/classrooms/new 必须命中新建页而不是课堂详情页', async () => {
    const router = createTestRouter()

    await router.push('/teacher/classrooms/new')

    expect(router.currentRoute.value.name).toBe('teacher-classroom-new')
  })

  it('动态路由把 :id 原样暴露给页面，便于详情页与监督台取用', async () => {
    const router = createTestRouter()

    await router.push('/teacher/classrooms/room-42/monitor')

    expect(router.currentRoute.value.name).toBe('teacher-classroom-monitor')
    expect(router.currentRoute.value.params.id).toBe('room-42')
  })

  it('不包含学生端 / 管理端路径：三个入口是物理分离的 SPA（§5）', () => {
    expect(routes.some((route) => route.path.startsWith('/student'))).toBe(false)
    expect(routes.some((route) => route.path.startsWith('/admin'))).toBe(false)
  })
})
