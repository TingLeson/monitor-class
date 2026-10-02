import { ApiError } from '@classwatch/api-client'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  makeClassroom,
  makeClassroomRun,
  makeClassroomStudent,
  makeOpenClassroom,
} from '../../__tests__/fixtures'
import { useClassroomsStore } from '../classrooms'

/**
 * 课堂 store 测试（§64 Frontend Test / §69）。
 *
 * 覆盖的全是"错了会静默骗人"的行为：失败被当成空列表、开启失败却显示已开启、
 * 批量添加把 rejected 丢掉、移除学生后名单还留着被移除的人、
 * 切换课堂时上一个课堂的名单还在 store 里。
 *
 * WHY mock 的是 lib/teacher-classrooms-api 而不是 fetch：这一层验证的是"拿到后端
 * 结果后 store 怎么处理"；HTTP 层（Cookie/CSRF/错误体解析）由
 * teacher-classrooms-api.spec.ts 与 packages/api-client 的测试覆盖。
 */
const {
  listClassroomsMock,
  getClassroomMock,
  createClassroomMock,
  updateClassroomMock,
  listStudentsMock,
  addStudentsMock,
  removeStudentMock,
  openClassroomMock,
  closeClassroomMock,
} = vi.hoisted(() => ({
  listClassroomsMock: vi.fn(),
  getClassroomMock: vi.fn(),
  createClassroomMock: vi.fn(),
  updateClassroomMock: vi.fn(),
  listStudentsMock: vi.fn(),
  addStudentsMock: vi.fn(),
  removeStudentMock: vi.fn(),
  openClassroomMock: vi.fn(),
  closeClassroomMock: vi.fn(),
}))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: getClassroomMock,
  createClassroom: createClassroomMock,
  updateClassroom: updateClassroomMock,
  listClassroomStudents: listStudentsMock,
  addClassroomStudents: addStudentsMock,
  removeClassroomStudent: removeStudentMock,
  openClassroom: openClassroomMock,
  closeClassroom: closeClassroomMock,
}))

/** 手写 deferred：用来制造"后发的请求先返回"的并发场景。 */
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => undefined
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

const CLOSED = makeClassroom({ id: 'room-closed', name: 'C++ 算法训练' })
const OPEN = makeOpenClassroom({ id: 'room-open', name: '数据结构练习' })
const STUDENT = makeClassroomStudent()

describe('课堂 store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listClassroomsMock.mockReset().mockResolvedValue([OPEN, CLOSED])
    getClassroomMock.mockReset().mockResolvedValue(CLOSED)
    createClassroomMock.mockReset()
    updateClassroomMock.mockReset()
    listStudentsMock.mockReset().mockResolvedValue({ students: [STUDENT] })
    addStudentsMock.mockReset()
    removeStudentMock.mockReset().mockResolvedValue(undefined)
    openClassroomMock.mockReset()
    closeClassroomMock.mockReset()
  })

  it('初始状态：未加载、无数据、无错误', () => {
    const store = useClassroomsStore()

    expect(store.items).toEqual([])
    expect(store.total).toBe(0)
    expect(store.hasLoaded).toBe(false)
    expect(store.isEmpty).toBe(false)
    expect(store.error).toBeNull()
    expect(store.current).toBeNull()
    expect(store.students).toEqual([])
  })

  it('加载成功：写入列表、hasLoaded 置真、开启中数量由列表推导', async () => {
    const store = useClassroomsStore()

    await store.fetchList()

    expect(store.items.map((item) => item.id)).toEqual(['room-open', 'room-closed'])
    expect(store.total).toBe(2)
    expect(store.openCount).toBe(1)
    expect(store.hasLoaded).toBe(true)
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
    expect(store.isEmpty).toBe(false)
  })

  it('列表为空且加载成功时才是空状态（失败不能显示"还没有课堂"）', async () => {
    listClassroomsMock.mockResolvedValue([])
    const store = useClassroomsStore()

    await store.fetchList()

    expect(store.isEmpty).toBe(true)
    expect(store.hasLoaded).toBe(true)
  })

  it('加载失败：记录 ApiError、不动 hasLoaded、不显示空状态', async () => {
    listClassroomsMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR' }))
    const store = useClassroomsStore()

    await store.fetchList()

    expect(store.error?.code).toBe('NETWORK_ERROR')
    expect(store.hasLoaded).toBe(false)
    expect(store.isEmpty).toBe(false)
    expect(store.loading).toBe(false)
  })

  it('非 ApiError 的异常也降级成 ApiError（视图只需处理一种错误类型）', async () => {
    listClassroomsMock.mockRejectedValue(new Error('boom'))
    const store = useClassroomsStore()

    await store.fetchList()

    expect(store.error?.code).toBe('INTERNAL')
  })

  it('并发刷新：慢的旧响应不覆盖新结果', async () => {
    const first = deferred<unknown>()
    const second = deferred<unknown>()
    listClassroomsMock.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const store = useClassroomsStore()

    const firstCall = store.fetchList()
    const secondCall = store.fetchList()
    // 后发的请求先返回，先发的请求后返回：最终状态必须是第二次的结果。
    second.resolve([OPEN])
    await secondCall
    first.resolve([])
    await firstCall

    expect(store.items.map((item) => item.id)).toEqual(['room-open'])
  })

  it('create：成功后重新拉列表（列表顺序与筛选由后端决定），并返回新课堂', async () => {
    const created = makeClassroom({ id: 'room-new', name: '新课堂' })
    createClassroomMock.mockResolvedValue(created)
    const store = useClassroomsStore()

    const result = await store.create({ name: '新课堂', description: null })

    expect(result.id).toBe('room-new')
    expect(createClassroomMock).toHaveBeenCalledWith({ name: '新课堂', description: null })
    expect(listClassroomsMock).toHaveBeenCalledTimes(1)
    expect(store.creating).toBe(false)
  })

  it('create 进行中 creating 为真（视图据此禁用提交按钮）', async () => {
    const pending = deferred<unknown>()
    createClassroomMock.mockReturnValue(pending.promise)
    const store = useClassroomsStore()

    const task = store.create({ name: '新课堂' })
    expect(store.creating).toBe(true)

    pending.resolve(makeClassroom())
    await task
    expect(store.creating).toBe(false)
  })

  it('open：用返回的 DTO 更新那一行，currentRun 变成这一次的新 Run（§8）', async () => {
    const opened = makeOpenClassroom({
      id: CLOSED.id,
      status: 'OPEN',
      studentCount: 2,
      currentRun: { id: 'run-9', openedAt: '2026-02-03T06:05:00Z' },
    })
    openClassroomMock.mockResolvedValue({
      classroom: opened,
      run: makeClassroomRun({ id: 'run-9', classroomId: CLOSED.id }),
    })
    const store = useClassroomsStore()
    await store.fetchList()

    const response = await store.open(CLOSED.id)

    const row = store.items.find((item) => item.id === CLOSED.id)
    expect(row?.status).toBe('OPEN')
    expect(row?.currentRun?.id).toBe('run-9')
    expect(response.run.id).toBe('run-9')
    expect(store.pendingId).toBeNull()
  })

  it('open 失败时列表保持后端原样（不做乐观更新）', async () => {
    openClassroomMock.mockRejectedValue(
      new ApiError({ code: 'CLASSROOM_ALREADY_OPEN', status: 409 }),
    )
    const store = useClassroomsStore()
    await store.fetchList()

    await expect(store.open(CLOSED.id)).rejects.toBeInstanceOf(ApiError)

    expect(store.items.find((item) => item.id === CLOSED.id)?.status).toBe('CLOSED')
    expect(store.pendingId).toBeNull()
  })

  it('open 进行中 pendingId 是那一行（只禁用这一行的按钮）', async () => {
    const pending = deferred<unknown>()
    openClassroomMock.mockReturnValue(pending.promise)
    const store = useClassroomsStore()

    const task = store.open(CLOSED.id)
    expect(store.pendingId).toBe(CLOSED.id)
    expect(store.pendingAction).toBe('open')

    pending.resolve({ classroom: makeOpenClassroom(), run: makeClassroomRun() })
    await task
    expect(store.pendingId).toBeNull()
    expect(store.pendingAction).toBeNull()
  })

  it('close：状态回到 CLOSED 且 currentRun 清空（§49 的控制面结果）', async () => {
    closeClassroomMock.mockResolvedValue({
      classroom: makeClassroom({ id: OPEN.id, status: 'CLOSED', currentRun: null }),
      run: makeClassroomRun({ status: 'CLOSED', closedAt: '2026-02-03T07:00:00Z' }),
    })
    const store = useClassroomsStore()
    await store.fetchList()

    await store.close(OPEN.id)

    const row = store.items.find((item) => item.id === OPEN.id)
    expect(row?.status).toBe('CLOSED')
    expect(row?.currentRun).toBeNull()
    expect(store.openCount).toBe(0)
  })

  it('详情与名单：加载后写入 current / students，并用名单长度同步 studentCount', async () => {
    listStudentsMock.mockResolvedValue({
      students: [STUDENT, makeClassroomStudent({ id: 'user-student-2', account: 'S10087' })],
    })
    const store = useClassroomsStore()
    await store.fetchList()

    await Promise.all([store.fetchDetail(CLOSED.id), store.fetchStudents(CLOSED.id)])

    expect(store.current?.id).toBe(CLOSED.id)
    expect(store.students.map((item) => item.account)).toEqual(['S10086', 'S10087'])
    expect(store.studentsLoaded).toBe(true)
    expect(store.items.find((item) => item.id === CLOSED.id)?.studentCount).toBe(2)
  })

  it('详情 404：清空 current 并记录错误码（页面据此给回列表入口）', async () => {
    getClassroomMock.mockRejectedValue(new ApiError({ code: 'CLASSROOM_NOT_FOUND', status: 404 }))
    const store = useClassroomsStore()
    await store.fetchDetail('room-closed')
    await store.fetchDetail(CLOSED.id)

    expect(store.currentError?.code).toBe('CLASSROOM_NOT_FOUND')
    expect(store.current).toBeNull()
    expect(store.currentLoading).toBe(false)
  })

  it('名单加载失败：清空名单且不把"没有学生"当成事实', async () => {
    listStudentsMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR' }))
    const store = useClassroomsStore()

    await store.fetchStudents(CLOSED.id)

    expect(store.studentsError?.code).toBe('NETWORK_ERROR')
    expect(store.students).toEqual([])
    expect(store.studentsLoaded).toBe(false)
  })

  it('addStudents：部分成功时保留 rejected，并用返回的完整名单刷新本地名单', async () => {
    addStudentsMock.mockResolvedValue({
      students: [STUDENT],
      rejected: [
        { account: 'S99999', code: 'STUDENT_NOT_FOUND' },
        { account: 'T1001', code: 'NOT_A_STUDENT' },
      ],
    })
    const store = useClassroomsStore()
    await store.fetchList()

    const result = await store.addStudents(CLOSED.id, ['S10086', 'S99999', 'T1001'])

    expect(result.acceptedCount).toBe(1)
    expect(result.rejected).toHaveLength(2)
    expect(store.lastAddResult?.rejected.map((item) => item.account)).toEqual(['S99999', 'T1001'])
    expect(store.students.map((item) => item.account)).toEqual(['S10086'])
    expect(store.items.find((item) => item.id === CLOSED.id)?.studentCount).toBe(1)
  })

  it('addStudents 被整批拒绝时抛出错误，且不留下上一次的结果（避免显示过期提示）', async () => {
    addStudentsMock.mockRejectedValue(new ApiError({ code: 'INVALID_REQUEST', status: 400 }))
    const store = useClassroomsStore()

    await expect(store.addStudents(CLOSED.id, [])).rejects.toBeInstanceOf(ApiError)

    expect(store.lastAddResult).toBeNull()
    expect(store.pendingId).toBeNull()
  })

  it('removeStudent：成功后重新拉名单（204 没有响应体，不以本地删除为准）', async () => {
    const store = useClassroomsStore()
    listStudentsMock
      .mockResolvedValueOnce({
        students: [STUDENT, makeClassroomStudent({ id: 'user-student-2' })],
      })
      .mockResolvedValueOnce({ students: [makeClassroomStudent({ id: 'user-student-2' })] })
    await store.fetchStudents(CLOSED.id)

    await store.removeStudent(CLOSED.id, STUDENT.id)

    expect(removeStudentMock).toHaveBeenCalledWith(CLOSED.id, STUDENT.id)
    expect(listStudentsMock).toHaveBeenCalledTimes(2)
    expect(store.students.map((item) => item.id)).toEqual(['user-student-2'])
    expect(store.removingStudentId).toBeNull()
  })

  it('removeStudent 失败（STUDENT_NOT_ASSIGNED）时名单不变，错误抛给调用方', async () => {
    removeStudentMock.mockRejectedValue(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 }))
    const store = useClassroomsStore()
    await store.fetchStudents(CLOSED.id)

    await expect(store.removeStudent(CLOSED.id, 'user-student-9')).rejects.toBeInstanceOf(ApiError)

    expect(store.students.map((item) => item.id)).toEqual([STUDENT.id])
    expect(listStudentsMock).toHaveBeenCalledTimes(1)
  })

  it('update：用返回的 DTO 替换那一行与详情头部', async () => {
    updateClassroomMock.mockResolvedValue(makeClassroom({ id: CLOSED.id, name: '改过的名字' }))
    const store = useClassroomsStore()
    await store.fetchList()
    await store.fetchDetail(CLOSED.id)

    await store.update(CLOSED.id, { name: '改过的名字' })

    expect(store.items.find((item) => item.id === CLOSED.id)?.name).toBe('改过的名字')
    expect(store.current?.name).toBe('改过的名字')
  })

  it('fetchList 会把详情头部同步成列表里的新状态（另一个标签页开课时不会显示过期状态）', async () => {
    const store = useClassroomsStore()
    await store.fetchList()
    await store.fetchDetail(CLOSED.id)
    expect(store.current?.status).toBe('CLOSED')

    listClassroomsMock.mockResolvedValue([makeOpenClassroom({ id: CLOSED.id })])
    await store.fetchList()

    expect(store.current?.status).toBe('OPEN')
  })

  it('clearDetail：清空详情、名单与添加结果，并让在飞行中的响应作废（§14 不跨课堂）', async () => {
    const pending = deferred<unknown>()
    getClassroomMock.mockReturnValue(pending.promise)
    const store = useClassroomsStore()
    await store.fetchStudents(CLOSED.id)
    expect(store.students).toHaveLength(1)

    const task = store.fetchDetail(CLOSED.id)
    store.clearDetail()
    pending.resolve(makeClassroom({ id: 'room-other', name: '另一个课堂' }))
    await task

    expect(store.current).toBeNull()
    expect(store.students).toEqual([])
    expect(store.studentsLoaded).toBe(false)
    expect(store.lastAddResult).toBeNull()
  })
})
