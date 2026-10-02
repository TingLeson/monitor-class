import { API_ERROR_MESSAGES } from '@classwatch/shared-types'
import { beforeEach, describe, expect, it } from 'vitest'
import { ApiError, isApiError } from '../api-error'
import { CSRF_HEADER, REQUEST_ID_HEADER, createApiClient } from '../client'
import { createCsrfTokenProvider, readCookie } from '../csrf'

const BASE_URL = 'https://api.example.com/api/v1'

interface FetchCall {
  url: string
  init: RequestInit
}

/** 用真实 Response 构造假响应：这样 headers/text()/ok/status 的语义与浏览器一致。 */
function createFetchStub(respond: (call: number) => Response | Promise<Response>): {
  calls: FetchCall[]
  fetchImpl: typeof fetch
} {
  const calls: FetchCall[] = []
  const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init: init ?? {} })
    return respond(calls.length)
  }) as typeof fetch
  return { calls, fetchImpl }
}

function jsonResponse(payload: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

function errorResponse(
  body: { code: string; message: string },
  status: number,
  requestId = 'rid-from-body',
) {
  return jsonResponse({ error: body, requestId }, status)
}

function headersOf(call: FetchCall): Record<string, string> {
  return (call.init.headers ?? {}) as Record<string, string>
}

describe('createApiClient', () => {
  it('GET 成功时解析 JSON，并始终携带 Cookie 会话与请求追踪头', async () => {
    const { calls, fetchImpl } = createFetchStub(() => jsonResponse([{ id: 'c1' }]))
    const client = createApiClient({
      baseUrl: BASE_URL,
      fetchImpl,
      generateRequestId: () => 'req-fixed',
    })

    const data = await client.get<{ id: string }[]>('/student/classrooms')

    expect(data).toEqual([{ id: 'c1' }])
    expect(calls).toHaveLength(1)
    expect(calls[0]?.url).toBe(`${BASE_URL}/student/classrooms`)
    expect(calls[0]?.init.method).toBe('GET')
    expect(calls[0]?.init.body).toBeUndefined()
    // 会话靠 HttpOnly Cookie 承载（§38/§41）：credentials 必须是 include。
    expect(calls[0]?.init.credentials).toBe('include')
    expect(headersOf(calls[0]!).Accept).toBe('application/json')
    expect(headersOf(calls[0]!)[REQUEST_ID_HEADER]).toBe('req-fixed')
  })

  it('POST 序列化请求体并声明 Content-Type', async () => {
    const { calls, fetchImpl } = createFetchStub(() => jsonResponse({ sessionId: 's1' }, 201))
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl })

    const result = await client.post<{ sessionId: string }>('/student/classrooms/c1/join', {
      capture: { displaySurface: 'monitor', width: 1920, height: 1080 },
    })

    expect(result.sessionId).toBe('s1')
    expect(calls[0]?.init.method).toBe('POST')
    expect(headersOf(calls[0]!)['Content-Type']).toBe('application/json')
    expect(JSON.parse(String(calls[0]?.init.body))).toEqual({
      capture: { displaySurface: 'monitor', width: 1920, height: 1080 },
    })
  })

  it('拼接查询参数：跳过 null/undefined，保留 false 与 0', async () => {
    const { calls, fetchImpl } = createFetchStub(() => jsonResponse({ ok: true }))
    const client = createApiClient({ baseUrl: `${BASE_URL}/`, fetchImpl })

    await client.get('/teacher/classrooms', {
      query: { status: 'OPEN', page: 0, archived: false, keyword: null, cursor: undefined },
    })

    const url = calls[0]?.url ?? ''
    expect(url.startsWith(`${BASE_URL}/teacher/classrooms?`)).toBe(true)
    expect(url.match(/\?/g)).toHaveLength(1)
    expect(url).toContain('status=OPEN')
    expect(url).toContain('page=0')
    expect(url).toContain('archived=false')
    expect(url).not.toContain('keyword')
    expect(url).not.toContain('cursor')
  })

  it('204 无响应体时返回 undefined（leave / delete 类接口）', async () => {
    const { fetchImpl } = createFetchStub(() => new Response(null, { status: 204 }))
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl })

    await expect(client.del('/teacher/classrooms/c1/students/s1')).resolves.toBeUndefined()
  })

  it('解析后端统一错误体（§58）为 ApiError，保留 code/status/requestId/message', async () => {
    const { fetchImpl } = createFetchStub(() =>
      errorResponse({ code: 'CLASSROOM_CLOSED', message: '课堂尚未开启' }, 409, 'rid-409'),
    )
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl })

    const error = await client.post('/student/classrooms/c1/join').catch((e: unknown) => e)

    expect(isApiError(error)).toBe(true)
    const apiError = error as ApiError
    expect(apiError.code).toBe('CLASSROOM_CLOSED')
    expect(apiError.status).toBe(409)
    expect(apiError.requestId).toBe('rid-409')
    expect(apiError.message).toBe('课堂尚未开启')
  })

  it('后端出现前端未知的错误码时降级为 INTERNAL，但保留可读 message', async () => {
    const { fetchImpl } = createFetchStub(() =>
      errorResponse({ code: 'SOMETHING_NEW_FROM_BACKEND', message: '新的业务错误' }, 400),
    )
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl })

    const error = (await client.get('/student/classrooms').catch((e: unknown) => e)) as ApiError

    expect(error.code).toBe('INTERNAL')
    expect(error.message).toBe('新的业务错误')
    expect(error.requestId).toBe('rid-from-body')
  })

  it('无法解析的 500 响应绝不把原始 HTML 透给界面（§58）', async () => {
    const { fetchImpl } = createFetchStub(
      () =>
        new Response('<html><body><h1>500 Internal Server Error</h1>nginx/1.27</body></html>', {
          status: 500,
          headers: { 'Content-Type': 'text/html' },
        }),
    )
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl })

    const error = (await client.get('/student/classrooms').catch((e: unknown) => e)) as ApiError

    expect(error.code).toBe('INTERNAL')
    expect(error.status).toBe(500)
    expect(error.message).not.toContain('html')
    expect(error.message).not.toContain('nginx')
    expect(error.message).not.toContain('500')
  })

  it('网络失败（fetch reject）映射为 NETWORK_ERROR 并保留原始 cause', async () => {
    const cause = new TypeError('Failed to fetch')
    const client = createApiClient({
      baseUrl: BASE_URL,
      fetchImpl: (() => Promise.reject(cause)) as unknown as typeof fetch,
      generateRequestId: () => 'req-net',
    })

    const error = (await client.get('/student/classrooms').catch((e: unknown) => e)) as ApiError

    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe('NETWORK_ERROR')
    // status 0 表示请求根本没到达服务器，UI 应提示网络而不是"服务器错误"。
    expect(error.status).toBe(0)
    expect(error.requestId).toBe('req-net')
    expect(error.cause).toBe(cause)
    expect(error.isNetworkError).toBe(true)
  })

  it('CSRF 钩子只注入写请求，GET 不受影响', async () => {
    const { calls, fetchImpl } = createFetchStub((call) =>
      call === 1 ? jsonResponse({ ok: true }) : new Response(null, { status: 204 }),
    )
    const client = createApiClient({
      baseUrl: BASE_URL,
      fetchImpl,
      csrfToken: () => 'csrf-token-1',
    })

    await client.get('/student/classrooms')
    await client.post('/teacher/classrooms/c1/open')

    expect(headersOf(calls[0]!)[CSRF_HEADER]).toBeUndefined()
    expect(headersOf(calls[1]!)[CSRF_HEADER]).toBe('csrf-token-1')
  })

  it('POST/PUT/PATCH/DELETE 一律带 CSRF 头，GET 一律不带', async () => {
    const { calls, fetchImpl } = createFetchStub(() => new Response(null, { status: 204 }))
    const client = createApiClient({
      baseUrl: BASE_URL,
      fetchImpl,
      csrfToken: () => 'csrf-token-1',
    })

    await client.get('/x')
    await client.post('/x')
    await client.patch('/x', { name: 'n' })
    await client.request('PUT', '/x', { body: { name: 'n' } })
    await client.del('/x')

    // 索引 0 是 GET；1..4 是所有非安全方法（§63：写请求必须带 CSRF token）。
    expect(headersOf(calls[0]!)[CSRF_HEADER]).toBeUndefined()
    for (const call of calls.slice(1)) {
      expect(headersOf(call)[CSRF_HEADER]).toBe('csrf-token-1')
      // credentials 必须保持 include：CSRF 是补充，不是会话载体的替代。
      expect(call.init.credentials).toBe('include')
    }
  })

  it('CSRF cookie 缺失（provider 返回 null）时不发送该头，绝不发空字符串', async () => {
    const { calls, fetchImpl } = createFetchStub(() => new Response(null, { status: 204 }))
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl, csrfToken: () => null })

    await client.post('/student/auth/login', { account: 'S10086' })

    // 缺头会让后端返回 CSRF_INVALID（403）；空字符串会让服务端无法区分
    // "没带 token"和"带了空 token"，排障时两种情况的结论完全不同。
    expect(CSRF_HEADER in headersOf(calls[0]!)).toBe(false)
  })

  it('CSRF 头每个写请求都重新读取 provider（登录后才下发的 cookie 必须能生效）', async () => {
    const { calls, fetchImpl } = createFetchStub(() => jsonResponse({ user: { id: 'u1' } }))
    let token: string | null = null
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl, csrfToken: () => token })

    await client.post('/student/auth/login', { account: 'S10086' })
    // 模拟后端在登录响应里 Set-Cookie 下发 CSRF cookie。
    token = 'csrf-after-login'
    await client.post('/student/classrooms/c1/join')

    expect(headersOf(calls[0]!)[CSRF_HEADER]).toBeUndefined()
    expect(headersOf(calls[1]!)[CSRF_HEADER]).toBe('csrf-after-login')
  })

  it('调用方可以覆盖 baseUrl 之外的同源路径与自定义 request id', async () => {
    const { calls, fetchImpl } = createFetchStub(() => jsonResponse({ ok: true }))
    const client = createApiClient({ baseUrl: '', fetchImpl })

    await client.get('/healthz', { requestId: 'trace-42' })

    expect(calls[0]?.url).toBe('/healthz')
    expect(headersOf(calls[0]!)[REQUEST_ID_HEADER]).toBe('trace-42')
  })

  it.each([
    ['INVALID_CREDENTIALS', 401],
    ['RATE_LIMITED', 429],
    ['CSRF_INVALID', 403],
    ['ACCOUNT_DISABLED', 403],
  ] as const)('解析 Phase 1 新增错误码 %s（HTTP %i）', async (code, status) => {
    const { fetchImpl } = createFetchStub(() => errorResponse({ code, message: '' }, status))
    const client = createApiClient({ baseUrl: BASE_URL, fetchImpl })

    const error = (await client.post('/teacher/auth/login').catch((e: unknown) => e)) as ApiError

    expect(error.code).toBe(code)
    expect(error.status).toBe(status)
    // 后端 message 为空时回落到 shared-types 的中文文案，绝不会是空提示。
    expect(error.message).toBe(API_ERROR_MESSAGES[code])
    expect(error.message.length).toBeGreaterThan(0)
  })
})

describe('createCsrfTokenProvider', () => {
  const COOKIE_NAME = 'classwatch_session_student_csrf'

  beforeEach(() => {
    // happy-dom 的 document.cookie 在同一个测试文件内是共享的，显式清空避免串味。
    for (const name of [COOKIE_NAME, 'classwatch_session_teacher_csrf', 'other', 'a', 'aa']) {
      document.cookie = `${name}=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/`
    }
  })

  it('按名字读取 CSRF cookie 的值', () => {
    document.cookie = `${COOKIE_NAME}=token-abc; path=/`
    const provider = createCsrfTokenProvider({ cookieName: COOKIE_NAME })

    expect(provider()).toBe('token-abc')
  })

  it('cookie 不存在时返回 null（而不是抛错或空串）', () => {
    const provider = createCsrfTokenProvider({ cookieName: COOKIE_NAME })

    expect(provider()).toBeNull()
  })

  it('不会被名字互为前缀的其它 cookie 误命中', () => {
    document.cookie = 'a=wrong; path=/'
    document.cookie = 'aa=x; path=/'

    expect(createCsrfTokenProvider({ cookieName: 'a' })()).toBe('wrong')
    expect(createCsrfTokenProvider({ cookieName: 'aa' })()).toBe('x')
    expect(createCsrfTokenProvider({ cookieName: 'aaa' })()).toBeNull()
  })

  it('每次都重新读取 document.cookie，不在创建时缓存', () => {
    const provider = createCsrfTokenProvider({ cookieName: COOKIE_NAME })
    expect(provider()).toBeNull()

    document.cookie = `${COOKIE_NAME}=late-token; path=/`

    expect(provider()).toBe('late-token')
  })

  it('HttpOnly 的会话 cookie 与 CSRF cookie 互不影响：provider 只认自己那一个名字', () => {
    // 模拟后端登录后同时下发会话 cookie（HttpOnly，JS 读不到）与 CSRF cookie。
    document.cookie = `${COOKIE_NAME}=csrf-value; path=/`

    const provider = createCsrfTokenProvider({ cookieName: COOKIE_NAME })

    expect(provider()).toBe('csrf-value')
    expect(readCookie('classwatch_session_student')).toBeNull()
  })
})
