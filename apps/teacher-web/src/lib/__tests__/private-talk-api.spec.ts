import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { errorBody } from '../../__tests__/fixtures.ts'
import { getPrivateTalk, startPrivateTalk, stopPrivateTalk } from '../private-talk-api.ts'

/**
 * 私密语音接口层测试（§31 / §76）。
 *
 * 与其它接口层测试同样的立场：**不 mock api-client**，注入 fake fetch，
 * 让真实的客户端把路径、Cookie、CSRF、错误体解析走一遍——只要有一处拼错，
 * 老师就会在课堂上收到一个 404，而那正是"点一下没反应"最难查的一种。
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

/** 204：DELETE 的真实形态（无响应体）。 */
function emptyResponse(status = 204) {
  return {
    ok: status >= 200 && status < 300,
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

describe('老师端私密语音接口层', () => {
  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('§31：POST /teacher/classrooms/:id/private-talk，body 是 {studentId}', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        target: { studentId: 'student-1', displayName: '张三', sessionId: 'session-1' },
      }),
    )

    const target = await startPrivateTalk('room-1', 'student-1')

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/teacher/classrooms/room-1/private-talk')
    expect(init.method).toBe('POST')
    expect(init.credentials).toBe('include')
    expect(JSON.parse(String(init.body))).toEqual({ studentId: 'student-1' })
    // 目标是**服务端**的答案（含它认定的 sessionId），前端不自己拼。
    expect(target).toEqual({ studentId: 'student-1', displayName: '张三', sessionId: 'session-1' })
  })

  it('§31：切换目标就是再 POST 一次（不需要先 DELETE）', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        target: { studentId: 'student-2', displayName: '李四', sessionId: 'session-2' },
      }),
    )

    const target = await startPrivateTalk('room-1', 'student-2')

    // 只有这一次请求：后端负责"先撤销旧的"（§31）。前端多发一次 DELETE
    // 只会在中间留下一个"没有目标"的窗口。
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(callAt().init.method).toBe('POST')
    expect(target?.studentId).toBe('student-2')
  })

  it('§31：DELETE 结束私密语音，204 无响应体', async () => {
    fetchMock.mockResolvedValue(emptyResponse())

    await expect(stopPrivateTalk('room-1')).resolves.toBeUndefined()

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/teacher/classrooms/room-1/private-talk')
    expect(init.method).toBe('DELETE')
  })

  it('§31：GET 读取当前目标；没有目标时是 null（而不是缺字段）', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ target: null }))

    await expect(getPrivateTalk('room-1')).resolves.toBeNull()

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/teacher/classrooms/room-1/private-talk')
    expect(init.method).toBe('GET')
  })

  it('§58：TEACHER_MIC_REQUIRED 原样抛出（由界面翻成"请先开启你的麦克风"）', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('TEACHER_MIC_REQUIRED'), 409))

    const failure = (await startPrivateTalk('room-1', 'student-1').catch(
      (error: unknown) => error,
    )) as { code?: string; status?: number }

    expect(failure.code).toBe('TEACHER_MIC_REQUIRED')
    expect(failure.status).toBe(409)
  })

  it('§58：PRIVATE_TALK_UNAVAILABLE（目标不在课堂）同样原样抛出', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('PRIVATE_TALK_UNAVAILABLE'), 409))

    const failure = (await startPrivateTalk('room-1', 'student-1').catch(
      (error: unknown) => error,
    )) as { code?: string }

    expect(failure.code).toBe('PRIVATE_TALK_UNAVAILABLE')
  })
})
