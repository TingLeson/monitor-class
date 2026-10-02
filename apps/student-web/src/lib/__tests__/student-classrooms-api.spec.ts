import { ApiError } from '@classwatch/api-client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { errorBody, makeOpenStudentClassroom, makeStudentClassroom } from '../../__tests__/fixtures'
import { getClassroom, listClassrooms } from '../student-classrooms-api'

/**
 * 学生端课堂接口层测试（§14 / §15 / §42 Student / §58）。
 *
 * 这里**不 mock api-client**：注入 fake fetch，让真实的客户端把 URL、Cookie 凭据、
 * 错误体解析都走一遍。"路径少了一段""信封拆错一层"这类 bug 只在接口层看得见。
 */
type FetchMock = ReturnType<typeof vi.fn>

let fetchMock: FetchMock

function jsonResponse(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers({ 'Content-Type': 'application/json' }),
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

function htmlResponse(body: string, status: number) {
  return {
    ok: false,
    status,
    headers: new Headers({ 'Content-Type': 'text/html' }),
    text: async () => body,
  } as unknown as Response
}

function callAt(index = 0): { url: string; init: RequestInit } {
  const call = fetchMock.mock.calls[index]
  if (!call) throw new Error(`fetch 未被调用第 ${index} 次`)
  return { url: String(call[0]), init: (call[1] ?? {}) as RequestInit }
}

describe('学生端课堂接口层', () => {
  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('列表：GET /student/classrooms，读请求不带 CSRF 头，拆开 classrooms 信封', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        classrooms: [makeOpenStudentClassroom(), makeStudentClassroom({ id: 'room-closed' })],
      }),
    )

    const classrooms = await listClassrooms()

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/student/classrooms')
    expect(init.method).toBe('GET')
    expect(init.credentials).toBe('include')
    expect((init.headers ?? {}) as Record<string, string>).not.toHaveProperty('X-CSRF-Token')
    expect(classrooms).toHaveLength(2)
  })

  it('列表原样返回后端结果：不筛选、不重排、CLOSED 的课堂也不会被丢掉（§14）', async () => {
    // 故意把 CLOSED 放在前面：任何"前端顺手把不可进入的课堂沉底/过滤掉"的实现
    // 都会让这个断言失败——那正是 §14 禁止的"前端自己决定学生能看到什么"。
    const payload = {
      classrooms: [
        makeStudentClassroom({ id: 'room-closed', name: '数据结构练习' }),
        makeOpenStudentClassroom({ id: 'room-open' }),
      ],
    }
    fetchMock.mockResolvedValue(jsonResponse(payload))

    const classrooms = await listClassrooms()

    expect(classrooms.map((item) => item.id)).toEqual(['room-closed', 'room-open'])
    expect(classrooms).toEqual(payload.classrooms)
  })

  it('列表响应里没有人数/名单类字段（§26 的 DTO 层面隔离）', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ classrooms: [makeOpenStudentClassroom()] }))

    const [classroom] = await listClassrooms()

    expect(classroom).toBeDefined()
    const keys = Object.keys(classroom as object)
    expect(keys.sort()).toEqual(
      ['createdAt', 'currentRun', 'description', 'id', 'name', 'status', 'teacher'].sort(),
    )
    // 老师对象只有 displayName：账号等管理端字段不下发给学生。
    expect(classroom?.teacher).toEqual({ displayName: '李老师' })
  })

  it('详情：GET /student/classrooms/:id，拆开 classroom 信封', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ classroom: makeOpenStudentClassroom() }))

    const classroom = await getClassroom('room-open')

    expect(callAt().url).toBe('/api/v1/student/classrooms/room-open')
    expect(classroom.status).toBe('OPEN')
    expect(classroom.currentRun?.id).toBe('run-1')
  })

  it('未授权 / 不存在：404 STUDENT_NOT_ASSIGNED 原样抛出，不降级成 INTERNAL', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('STUDENT_NOT_ASSIGNED'), 404))

    const failure = (await getClassroom('room-x').catch((error: unknown) => error)) as ApiError

    expect(failure).toBeInstanceOf(ApiError)
    expect(failure.code).toBe('STUDENT_NOT_ASSIGNED')
    expect(failure.status).toBe(404)
    expect(failure.requestId).toBe('rid-test')
  })

  it('网关返回 500 HTML 时只暴露通用文案，绝不透出原始报文（§58）', async () => {
    fetchMock.mockResolvedValue(
      htmlResponse('<html><body>500 Internal Server Error</body></html>', 500),
    )

    const failure = (await listClassrooms().catch((error: unknown) => error)) as ApiError

    expect(failure.code).toBe('INTERNAL')
    expect(failure.message).not.toContain('Internal Server Error')
    expect(failure.message).not.toContain('<html>')
  })

  it('网络失败映射成 NETWORK_ERROR，而不是"未登录"或"没有课堂"', async () => {
    fetchMock.mockRejectedValue(new TypeError('Failed to fetch'))

    const failure = (await listClassrooms().catch((error: unknown) => error)) as ApiError

    expect(failure.code).toBe('NETWORK_ERROR')
    expect(failure.status).toBe(0)
  })

  it('列表把 AbortSignal 透传给 fetch（离开页面时可以取消在途请求）', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ classrooms: [] }))
    const controller = new AbortController()

    await listClassrooms({ signal: controller.signal })

    expect(callAt().init.signal).toBe(controller.signal)
  })

  it('本 Phase 只封装两个只读接口，不预置 join / media-token（§70 边界）', async () => {
    // 提前放一个 join / media-token 封装，下一个 Phase 极易顺手调用它，
    // 而它背后此刻根本没有后端实现——"点了没反应"就是这么来的。
    const apiModule = await import('../student-classrooms-api')

    expect(Object.keys(apiModule).sort()).toEqual(['getClassroom', 'listClassrooms'])
  })
})
