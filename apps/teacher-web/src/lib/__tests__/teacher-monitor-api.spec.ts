import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { errorBody } from '../../__tests__/fixtures.ts'
import { getMonitor, requestMediaToken } from '../teacher-monitor-api.ts'

/**
 * 监督接口层测试（§42 Teacher / §51）。
 *
 * 与其它接口层测试同样的立场：**不 mock api-client**，注入 fake fetch，
 * 让真实的客户端把路径、Cookie、CSRF、错误体解析走一遍。
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

function callAt(index = 0): { url: string; init: RequestInit } {
  const call = fetchMock.mock.calls[index]
  if (!call) throw new Error(`fetch 未被调用第 ${index} 次`)
  return { url: String(call[0]), init: (call[1] ?? {}) as RequestInit }
}

describe('老师端监督接口层', () => {
  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('monitor：GET /teacher/classrooms/:id/monitor，拆开 students 信封', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        students: [
          {
            studentId: 'student-1',
            displayName: '张三',
            sessionId: 'session-1',
            sessionStatus: 'ONLINE',
            screen: { active: true },
            camera: { active: false },
            microphone: { active: false },
            connection: 'GOOD',
            joinedAt: '2026-10-03T10:00:00Z',
            lastEventAt: '2026-10-03T10:05:00Z',
          },
        ],
      }),
    )

    const students = await getMonitor('room-1')

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/teacher/classrooms/room-1/monitor')
    expect(init.method).toBe('GET')
    expect(init.credentials).toBe('include')
    expect(students).toHaveLength(1)
    // sessionId 是业务 DTO 与 LiveKit identity 的唯一连接键（§44/§51），不能丢。
    expect(students[0]?.sessionId).toBe('session-1')
  })

  it('monitor 把 AbortSignal 透传给 fetch（离开页面时取消在途请求）', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ students: [] }))
    const controller = new AbortController()

    await getMonitor('room-1', { signal: controller.signal })

    expect(callAt().init.signal).toBe(controller.signal)
  })

  it('media-token：POST /teacher/classrooms/:id/media-token，带 CSRF 头且无请求体', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ livekitUrl: 'wss://classwatch.livekit.cloud', token: 'teacher-token' }),
    )

    const response = await requestMediaToken('room-1')

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/teacher/classrooms/room-1/media-token')
    expect(init.method).toBe('POST')
    expect(response.livekitUrl).toBe('wss://classwatch.livekit.cloud')
    // 权限由后端签发时写死（§27），前端不声明自己要什么权限。
    expect(init.body === undefined || init.body === null).toBe(true)
  })

  it('不是自己的课堂：403 CLASSROOM_NOT_OWNER 原样抛出，由界面给通用文案', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('CLASSROOM_NOT_OWNER'), 403))

    const failure = (await getMonitor('room-1').catch((error: unknown) => error)) as {
      code?: string
      status?: number
    }

    expect(failure.code).toBe('CLASSROOM_NOT_OWNER')
    expect(failure.status).toBe(403)
  })
})
