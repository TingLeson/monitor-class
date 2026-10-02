import { ApiError } from '@classwatch/api-client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { errorBody, makeStudent, makeTeacher } from '../../__tests__/fixtures'
import { CSRF_COOKIE_NAME } from '../api'
import {
  createUser,
  getUser,
  listUsers,
  resetTeacherPassword,
  updateDisplayName,
  updateUserStatus,
} from '../admin-users-api'

/**
 * 管理端用户接口层测试（§42 Admin / §58 / §63）。
 *
 * 这里**不 mock api-client**：注入 fake fetch，让真实的客户端把 URL、query、CSRF 头、
 * JSON 请求体与错误体解析都走一遍。只 mock HTTP 层的话，"写请求忘了带 CSRF 头"
 * 或"query 拼错字段名"这类 bug 会从两层测试的缝里漏过去。
 */
type FetchMock = ReturnType<typeof vi.fn>

let fetchMock: FetchMock

function jsonResponse(body: unknown, status = 200, headers: Record<string, string> = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers({ 'Content-Type': 'application/json', ...headers }),
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

function emptyResponse(status: number) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    text: async () => '',
  } as unknown as Response
}

/** 取出第 index 次 fetch 调用的 URL 与 init，省得每个用例都写一遍类型断言。 */
function callAt(index = 0): { url: string; init: RequestInit } {
  const call = fetchMock.mock.calls[index]
  if (!call) throw new Error(`fetch 未被调用第 ${index} 次`)
  return { url: String(call[0]), init: (call[1] ?? {}) as RequestInit }
}

function headersAt(index = 0): Record<string, string> {
  return (callAt(index).init.headers ?? {}) as Record<string, string>
}

describe('管理端用户接口层', () => {
  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    // CSRF token 由后端下发的非 HttpOnly Cookie 提供（§63）；测试直接写 cookie 模拟已登录。
    document.cookie = `${CSRF_COOKIE_NAME}=csrf-token-123`
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    document.cookie = `${CSRF_COOKIE_NAME}=; expires=Thu, 01 Jan 1970 00:00:00 GMT`
  })

  it('列表：拼出筛选与分页 query，空值不发送，GET 不带 CSRF 头', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ users: [makeTeacher()], total: 1, page: 2, pageSize: 50 }),
    )

    const response = await listUsers({ role: 'TEACHER', q: 'li', page: 2, pageSize: 50 })

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/admin/users?role=TEACHER&q=li&page=2&pageSize=50')
    expect(init.method).toBe('GET')
    expect(init.credentials).toBe('include')
    // GET 是安全方法：带 token 只会进浏览器缓存键与访问日志。
    expect(headersAt()['X-CSRF-Token']).toBeUndefined()
    expect(response.total).toBe(1)
  })

  it('创建账号：POST 到 /admin/users，带 CSRF 头与 JSON 请求体', async () => {
    fetchMock.mockResolvedValue(jsonResponse(makeTeacher({ account: 'T1001' }), 201))

    const created = await createUser({
      account: 'T1001',
      displayName: '李老师',
      role: 'TEACHER',
      password: 'a-strong-password',
    })

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/admin/users')
    expect(init.method).toBe('POST')
    expect(headersAt()['X-CSRF-Token']).toBe('csrf-token-123')
    expect(JSON.parse(String(init.body))).toEqual({
      account: 'T1001',
      displayName: '李老师',
      role: 'TEACHER',
      password: 'a-strong-password',
    })
    expect(created.account).toBe('T1001')
  })

  it('查看单个账号与编辑显示名都兼容两种成功信封（裸 DTO 与 { user }）', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(makeStudent()))
      .mockResolvedValueOnce(jsonResponse({ user: makeTeacher({ displayName: '新名字' }) }))

    expect((await getUser('user-student-1')).account).toBe('S10086')
    // PATCH 的响应体形状未写死：这里验证 { user } 信封同样能被拆开。
    expect(
      (await updateDisplayName('user-teacher-1', { displayName: '新名字' }))?.displayName,
    ).toBe('新名字')

    expect(callAt(0).url).toBe('/api/v1/admin/users/user-student-1')
    expect(callAt(1).url).toBe('/api/v1/admin/users/user-teacher-1')
    expect(callAt(1).init.method).toBe('PATCH')
    expect(JSON.parse(String(callAt(1).init.body))).toEqual({ displayName: '新名字' })
  })

  it('编辑显示名/改状态返回 204 时得到 null，而不是一个半成品对象', async () => {
    fetchMock.mockResolvedValue(emptyResponse(204))

    expect(await updateDisplayName('user-teacher-1', { displayName: 'x' })).toBeNull()
    expect(await updateUserStatus('user-teacher-1', { status: 'DISABLED' })).toBeNull()

    expect(callAt(1).url).toBe('/api/v1/admin/users/user-teacher-1/status')
    expect(JSON.parse(String(callAt(1).init.body))).toEqual({ status: 'DISABLED' })
  })

  it('重置老师密码：路径是 /admin/teachers/:id/reset-password，留空时提交空对象', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ user: makeTeacher(), password: 'server-generated-pw' }, 200),
    )

    const response = await resetTeacherPassword('user-teacher-1')

    expect(callAt().url).toBe('/api/v1/admin/teachers/user-teacher-1/reset-password')
    expect(callAt().init.method).toBe('POST')
    expect(JSON.parse(String(callAt().init.body))).toEqual({})
    expect(response.password).toBe('server-generated-pw')
  })

  it('新增的错误码被识别为业务码，而不是降级成 INTERNAL（§58）', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(errorBody('ACCOUNT_ALREADY_EXISTS', '该账号已存在'), 409, {
        'X-Request-Id': 'rid-header',
      }),
    )

    const failure = await createUser({
      account: 'S10086',
      displayName: '张三',
      role: 'STUDENT',
    }).catch((error: unknown) => error)

    expect(failure).toBeInstanceOf(ApiError)
    expect((failure as ApiError).code).toBe('ACCOUNT_ALREADY_EXISTS')
    expect((failure as ApiError).status).toBe(409)
    // requestId 一路带到 UI，排障时才能和后端结构化日志对齐（§59）。
    expect((failure as ApiError).requestId).toBe('rid-test')
  })

  it('PASSWORD_POLICY_VIOLATION 与 USER_NOT_FOUND 同样是已知业务码', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(errorBody('PASSWORD_POLICY_VIOLATION'), 400))
      .mockResolvedValueOnce(jsonResponse(errorBody('USER_NOT_FOUND'), 404))

    const policy = await createUser({
      account: 'T1001',
      displayName: '李老师',
      role: 'TEACHER',
      password: 'a-strong-password',
    }).catch((error: unknown) => error)
    const missing = await getUser('missing').catch((error: unknown) => error)

    expect((policy as ApiError).code).toBe('PASSWORD_POLICY_VIOLATION')
    expect((missing as ApiError).code).toBe('USER_NOT_FOUND')
  })

  it('后端返回 500 HTML 时不透出原始报文（§58）', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      headers: new Headers(),
      text: async () => '<html>500 Internal Server Error</html>',
    } as unknown as Response)

    const failure = (await listUsers().catch((error: unknown) => error)) as ApiError

    expect(failure.code).toBe('INTERNAL')
    expect(failure.message).not.toContain('html')
    expect(failure.message).not.toContain('Internal Server Error')
  })
})
