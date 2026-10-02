import { ApiError } from '@classwatch/api-client'
import type { StudentClassroom } from '@classwatch/shared-types'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeOpenStudentClassroom, makeStudentClassroom } from '../../__tests__/fixtures'
import { useClassroomsStore } from '../classrooms'

/**
 * 学生端课堂 store 测试（§64 Frontend Test）。
 *
 * 覆盖的全是"错了会静默骗人"的行为：加载失败被当成"没有课堂"、详情 404 后
 * 页面上还留着上一个课堂、切换课堂时旧数据先渲染出新名字。
 *
 * WHY mock 的是 lib/student-classrooms-api 而不是 fetch：这一层验证的是"拿到后端
 * 结果后 store 怎么处理"；HTTP 层（路径、Cookie、错误体解析）由
 * student-classrooms-api.spec.ts 与 packages/api-client 的测试覆盖。
 */
const { listClassroomsMock, getClassroomMock } = vi.hoisted(() => ({
  listClassroomsMock: vi.fn(),
  getClassroomMock: vi.fn(),
}))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: listClassroomsMock,
  getClassroom: getClassroomMock,
}))

/** 手写 deferred：用来制造"后发的请求先返回"和"清理后旧响应才到"的场景。 */
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => undefined
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

const OPEN = makeOpenStudentClassroom({ id: 'room-open', name: '数据结构练习' })
const CLOSED = makeStudentClassroom({ id: 'room-closed', name: 'C++ 算法训练' })

describe('学生端课堂 store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listClassroomsMock.mockReset().mockResolvedValue([OPEN, CLOSED])
    getClassroomMock.mockReset().mockResolvedValue(OPEN)
  })

  it('初始状态：未加载、无数据、无错误', () => {
    const store = useClassroomsStore()

    expect(store.items).toEqual([])
    expect(store.loading).toBe(false)
    expect(store.hasLoaded).toBe(false)
    expect(store.isEmpty).toBe(false)
    expect(store.error).toBeNull()
    expect(store.current).toBeNull()
    expect(store.currentLoading).toBe(false)
    expect(store.currentError).toBeNull()
  })

  it('列表加载成功：写入 items、hasLoaded 置真、顺序按后端返回（前端不重排）', async () => {
    const store = useClassroomsStore()

    await store.fetchList()

    expect(store.items.map((item) => item.id)).toEqual(['room-open', 'room-closed'])
    expect(store.hasLoaded).toBe(true)
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
    expect(store.isEmpty).toBe(false)
  })

  it('空列表且加载成功时才是空状态（失败不能显示"还没有被加入任何课堂"）', async () => {
    listClassroomsMock.mockResolvedValue([])
    const store = useClassroomsStore()

    await store.fetchList()

    expect(store.isEmpty).toBe(true)
    expect(store.hasLoaded).toBe(true)
  })

  it('列表加载失败：记录 ApiError、不动 hasLoaded、不进入空状态', async () => {
    listClassroomsMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const store = useClassroomsStore()

    await store.fetchList()

    expect(store.error?.code).toBe('NETWORK_ERROR')
    expect(store.hasLoaded).toBe(false)
    expect(store.isEmpty).toBe(false)
    expect(store.items).toEqual([])
    expect(store.loading).toBe(false)
  })

  it('列表失败后重试成功：错误被清掉，数据出现', async () => {
    listClassroomsMock.mockRejectedValueOnce(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    const store = useClassroomsStore()
    await store.fetchList()
    expect(store.error).not.toBeNull()

    await store.fetchList()

    expect(store.error).toBeNull()
    expect(store.items).toHaveLength(2)
  })

  it('详情加载成功：写入 current、清掉上一次的错误', async () => {
    const store = useClassroomsStore()
    getClassroomMock.mockRejectedValueOnce(new ApiError({ code: 'NETWORK_ERROR', status: 0 }))
    await store.fetchDetail('room-open')
    expect(store.currentError).not.toBeNull()

    await store.fetchDetail('room-open')

    expect(store.current?.id).toBe('room-open')
    expect(store.current?.name).toBe('数据结构练习')
    expect(store.currentError).toBeNull()
    expect(store.currentLoading).toBe(false)
    expect(getClassroomMock).toHaveBeenLastCalledWith('room-open')
  })

  it('详情 404（不在名单里）：清空 current，绝不留下上一次的数据', async () => {
    const store = useClassroomsStore()
    await store.fetchDetail('room-open')
    expect(store.current).not.toBeNull()

    getClassroomMock.mockRejectedValue(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 }))
    await store.fetchDetail('room-x')

    expect(store.current).toBeNull()
    expect(store.currentError?.code).toBe('STUDENT_NOT_ASSIGNED')
    expect(store.currentLoading).toBe(false)
  })

  it('从 A 切到 B 且 B 失败：current 是 null，而不是继续显示 A', async () => {
    const store = useClassroomsStore()
    const A = makeOpenStudentClassroom({ id: 'room-a', name: 'A 课堂' })
    const B = makeStudentClassroom({ id: 'room-b', name: 'B 课堂' })
    getClassroomMock.mockImplementation((id: string) =>
      id === 'room-a'
        ? Promise.resolve(A)
        : Promise.reject(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 })),
    )

    await store.fetchDetail(A.id)
    await store.fetchDetail(B.id)

    expect(store.current).toBeNull()
    expect(store.currentError?.code).toBe('STUDENT_NOT_ASSIGNED')
  })

  it('慢的旧响应不能覆盖新课堂：先发 A（慢）、再发 B，最终必须是 B', async () => {
    const store = useClassroomsStore()
    const slowA = deferred<StudentClassroom>()
    const A = makeOpenStudentClassroom({ id: 'room-a', name: 'A 课堂' })
    const B = makeStudentClassroom({ id: 'room-b', name: 'B 课堂' })
    getClassroomMock.mockImplementation((id: string) =>
      id === 'room-a' ? slowA.promise : Promise.resolve(B),
    )

    const firstRequest = store.fetchDetail(A.id)
    const secondRequest = store.fetchDetail(B.id)
    await secondRequest
    expect(store.current?.id).toBe('room-b')

    // A 的响应此刻才到：它已经过期了，不允许把页面改回 A。
    slowA.resolve(A)
    await firstRequest

    expect(store.current?.id).toBe('room-b')
  })

  it('clearDetail：清空详情与错误，并作废仍在飞行中的响应', async () => {
    const store = useClassroomsStore()
    const slow = deferred<StudentClassroom>()
    getClassroomMock.mockReturnValue(slow.promise)

    const pending = store.fetchDetail('room-a')
    store.clearDetail()

    expect(store.current).toBeNull()
    expect(store.currentError).toBeNull()
    expect(store.currentLoading).toBe(false)

    // 请求在清理之后才返回：它属于上一个课堂，必须被丢弃。
    slow.resolve(makeOpenStudentClassroom({ id: 'room-a' }))
    await pending

    expect(store.current).toBeNull()
  })
})
