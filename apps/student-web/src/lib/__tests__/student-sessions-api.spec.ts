import { ApiError } from '@classwatch/api-client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { errorBody } from '../../__tests__/fixtures'
import { joinClassroom, leaveSession } from '../student-sessions-api.ts'

/**
 * 学生"进入 / 离开课堂"接口层测试（§42 Student / §43 / §58）。
 *
 * 与课堂门户接口同样的立场：**不 mock api-client**，注入 fake fetch，让真实的
 * 客户端把 URL、Cookie、CSRF、错误体解析都走一遍。这里最要紧的两条断言是
 * "join 的请求体只有 §43 冻结的三个字段"（多一个字段就可能被后端 400 拒绝）
 * 与"leave 是 204 无响应体"（多解析一层就会在成功路径上抛错）。
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

function emptyResponse(status = 204) {
  return {
    ok: true,
    status,
    headers: new Headers(),
    text: async () => '',
  } as unknown as Response
}

function callAt(index = 0): { url: string; init: RequestInit } {
  const call = fetchMock.mock.calls[index]
  if (!call) throw new Error(`fetch 未被调用第 ${index} 次`)
  return { url: String(call[0]), init: (call[1] ?? {}) as RequestInit }
}

function bodyAt(index = 0): unknown {
  const raw = callAt(index).init.body
  return typeof raw === 'string' ? JSON.parse(raw) : raw
}

const JOIN_RESPONSE = {
  sessionId: 'session-1',
  livekitUrl: 'wss://classwatch.livekit.cloud',
  token: 'short-lived-token',
}

describe('学生端会话接口层', () => {
  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('join：POST /student/classrooms/:id/join，带 capture 诊断与 CSRF 头', async () => {
    fetchMock.mockResolvedValue(jsonResponse(JOIN_RESPONSE))

    const response = await joinClassroom('room-open', {
      displaySurface: 'monitor',
      width: 2560,
      height: 1440,
    })

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/student/classrooms/room-open/join')
    expect(init.method).toBe('POST')
    expect(init.credentials).toBe('include')
    expect(bodyAt()).toEqual({
      capture: { displaySurface: 'monitor', width: 2560, height: 1440 },
    })
    expect(response).toEqual(JOIN_RESPONSE)
  })

  it('join 请求体只有 §43 冻结的三个诊断字段（多余字段会被后端 400）', async () => {
    fetchMock.mockResolvedValue(jsonResponse(JOIN_RESPONSE))

    await joinClassroom('room-open', { displaySurface: 'monitor', width: 1920, height: 1080 })

    const body = bodyAt() as { capture: Record<string, unknown> }
    expect(Object.keys(body).sort()).toEqual(['capture'])
    expect(Object.keys(body.capture).sort()).toEqual(['displaySurface', 'height', 'width'])
  })

  it('join 失败：409 CLASSROOM_CLOSED 原样抛出（调用方据此停止共享）', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('CLASSROOM_CLOSED'), 409))

    const failure = (await joinClassroom('room-open', {
      displaySurface: 'monitor',
      width: 1,
      height: 1,
    }).catch((error: unknown) => error)) as ApiError

    expect(failure).toBeInstanceOf(ApiError)
    expect(failure.code).toBe('CLASSROOM_CLOSED')
    expect(failure.status).toBe(409)
  })

  it('leave：POST /student/sessions/:id/leave，204 无响应体也不报错', async () => {
    fetchMock.mockResolvedValue(emptyResponse())

    await expect(leaveSession('session-1')).resolves.toBeUndefined()

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/student/sessions/session-1/leave')
    expect(init.method).toBe('POST')
  })
})
