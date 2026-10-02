import { ApiError } from '@classwatch/api-client'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import {
  errorBody,
  makeClassroom,
  makeClassroomRun,
  makeClassroomStudent,
  makeOpenClassroom,
} from '../../__tests__/fixtures'
import { CSRF_COOKIE_NAME } from '../api'
import {
  addClassroomStudents,
  closeClassroom,
  createClassroom,
  getClassroom,
  listClassroomStudents,
  listClassrooms,
  openClassroom,
  removeClassroomStudent,
  updateClassroom,
} from '../teacher-classrooms-api'

/**
 * 老师端课堂接口层测试（§42 Teacher / §58 / §63）。
 *
 * 这里**不 mock api-client**：注入 fake fetch，让真实的客户端把 URL、CSRF 头、
 * JSON 请求体与错误体解析都走一遍。"写请求忘了带 CSRF 头""路径少了一段"
 * 这类 bug 只在一层测试里是看不出来的。
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

function callAt(index = 0): { url: string; init: RequestInit } {
  const call = fetchMock.mock.calls[index]
  if (!call) throw new Error(`fetch 未被调用第 ${index} 次`)
  return { url: String(call[0]), init: (call[1] ?? {}) as RequestInit }
}

function headersAt(index = 0): Record<string, string> {
  return (callAt(index).init.headers ?? {}) as Record<string, string>
}

describe('老师端课堂接口层', () => {
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

  it('列表：GET /teacher/classrooms，不带 CSRF 头，拆开 classrooms 信封', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ classrooms: [makeOpenClassroom()] }))

    const classrooms = await listClassrooms()

    expect(callAt().url).toBe('/api/v1/teacher/classrooms')
    expect(callAt().init.method).toBe('GET')
    expect(headersAt()['X-CSRF-Token']).toBeUndefined()
    expect(classrooms).toHaveLength(1)
    expect(classrooms[0]?.status).toBe('OPEN')
  })

  it('创建：POST 带 CSRF 头与 JSON 请求体，只提交 name / description（§10 不带 owner）', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ classroom: makeClassroom() }, 201))

    const created = await createClassroom({ name: 'C++ 算法训练', description: '第三章' })

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/teacher/classrooms')
    expect(init.method).toBe('POST')
    expect(headersAt()['X-CSRF-Token']).toBe('csrf-token-123')
    expect(JSON.parse(String(init.body))).toEqual({ name: 'C++ 算法训练', description: '第三章' })
    expect(created.id).toBe('room-1')
  })

  it('详情与编辑：/teacher/classrooms/:id，PATCH 只带改动字段', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ classroom: makeClassroom() }))
      .mockResolvedValueOnce(jsonResponse({ classroom: makeClassroom({ name: '新名字' }) }))

    await getClassroom('room-1')
    const updated = await updateClassroom('room-1', { name: '新名字' })

    expect(callAt(0).url).toBe('/api/v1/teacher/classrooms/room-1')
    expect(callAt(1).url).toBe('/api/v1/teacher/classrooms/room-1')
    expect(callAt(1).init.method).toBe('PATCH')
    expect(JSON.parse(String(callAt(1).init.body))).toEqual({ name: '新名字' })
    expect(updated.name).toBe('新名字')
  })

  it('学生名单：GET /teacher/classrooms/:id/students 拆开 students 信封', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ students: [makeClassroomStudent()] }))

    const response = await listClassroomStudents('room-1')

    expect(callAt().url).toBe('/api/v1/teacher/classrooms/room-1/students')
    expect(response.students[0]?.account).toBe('S10086')
  })

  it('批量添加：POST accounts 数组，部分成功的结果原样返回（students + rejected）', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        students: [makeClassroomStudent()],
        rejected: [{ account: 'S99999', code: 'STUDENT_NOT_FOUND' }],
      }),
    )

    const response = await addClassroomStudents('room-1', ['S10086', 'S99999'])

    expect(callAt().url).toBe('/api/v1/teacher/classrooms/room-1/students')
    expect(callAt().init.method).toBe('POST')
    expect(JSON.parse(String(callAt().init.body))).toEqual({ accounts: ['S10086', 'S99999'] })
    expect(response.students).toHaveLength(1)
    expect(response.rejected).toEqual([{ account: 'S99999', code: 'STUDENT_NOT_FOUND' }])
  })

  it('批量添加里的新错误码是已知业务码，不会被降级成 INTERNAL（§58）', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('NOT_A_STUDENT', '该账号不是学生账号'), 400))

    const failure = (await addClassroomStudents('room-1', ['T1001']).catch(
      (error: unknown) => error,
    )) as ApiError

    expect(failure.code).toBe('NOT_A_STUDENT')
    expect(failure.status).toBe(400)
  })

  it('移除学生：DELETE 到 /students/:studentId，204 视为成功（无响应体）', async () => {
    fetchMock.mockResolvedValue(emptyResponse(204))

    await expect(removeClassroomStudent('room-1', 'user-student-1')).resolves.toBeUndefined()

    const { url, init } = callAt()
    expect(url).toBe('/api/v1/teacher/classrooms/room-1/students/user-student-1')
    expect(init.method).toBe('DELETE')
    expect(headersAt()['X-CSRF-Token']).toBe('csrf-token-123')
  })

  it('移除不在名单里的学生：404 STUDENT_NOT_ASSIGNED 原样抛出，不当成成功', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('STUDENT_NOT_ASSIGNED'), 404))

    const failure = (await removeClassroomStudent('room-1', 'user-student-2').catch(
      (error: unknown) => error,
    )) as ApiError

    expect(failure).toBeInstanceOf(ApiError)
    expect(failure.code).toBe('STUDENT_NOT_ASSIGNED')
    expect(failure.status).toBe(404)
  })

  it('开启课堂：POST /open 返回 classroom + run（§48）', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        classroom: makeOpenClassroom({
          currentRun: { id: 'run-1', openedAt: '2026-02-03T06:05:00Z' },
        }),
        run: makeClassroomRun(),
      }),
    )

    const response = await openClassroom('room-1')

    expect(callAt().url).toBe('/api/v1/teacher/classrooms/room-1/open')
    expect(callAt().init.method).toBe('POST')
    expect(response.classroom.status).toBe('OPEN')
    expect(response.classroom.currentRun?.id).toBe('run-1')
    expect(response.run.status).toBe('OPEN')
  })

  it('关闭课堂：POST /close 返回 CLOSED 的课堂与结束的 run（§49）', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        classroom: makeClassroom({ status: 'CLOSED', currentRun: null }),
        run: makeClassroomRun({ status: 'CLOSED', closedAt: '2026-02-03T07:00:00Z' }),
      }),
    )

    const response = await closeClassroom('room-1')

    expect(callAt().url).toBe('/api/v1/teacher/classrooms/room-1/close')
    expect(response.classroom.status).toBe('CLOSED')
    expect(response.classroom.currentRun).toBeNull()
    expect(response.run.closedAt).toBe('2026-02-03T07:00:00Z')
  })

  it('409 冲突码是已知业务码（界面据此刷新列表而不是报"操作失败"）', async () => {
    fetchMock.mockResolvedValue(jsonResponse(errorBody('CLASSROOM_ALREADY_OPEN'), 409))

    const failure = (await openClassroom('room-1').catch((error: unknown) => error)) as ApiError

    expect(failure.code).toBe('CLASSROOM_ALREADY_OPEN')
    expect(failure.status).toBe(409)
  })

  it('后端 500 HTML 时不透出原始报文（§58）', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      headers: new Headers(),
      text: async () => '<html>500 Internal Server Error</html>',
    } as unknown as Response)

    const failure = (await listClassrooms().catch((error: unknown) => error)) as ApiError

    expect(failure.code).toBe('INTERNAL')
    expect(failure.message).not.toContain('html')
    expect(failure.message).not.toContain('Internal Server Error')
  })
})
