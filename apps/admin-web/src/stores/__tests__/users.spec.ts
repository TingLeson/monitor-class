import { ApiError } from '@classwatch/api-client'
import type { AdminUserListResponse } from '@classwatch/shared-types'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  makeAdminUser,
  makeStudent,
  makeTeacher,
  makeUserListResponse,
} from '../../__tests__/fixtures'
import { useUsersStore } from '../users'

/**
 * 用户列表 store 测试（§64 Frontend Test / §68）。
 *
 * 覆盖的全是"错了会静默骗人"的行为：失败被当成空列表、筛选后停在旧页码、
 * 乐观更新假装成功、慢响应覆盖新结果、一次性密码被留在状态里。
 *
 * WHY mock 的是 lib/admin-users-api 而不是 fetch：这一层要验证的是"拿到后端结果后
 * store 怎么处理"；HTTP 层（Cookie/CSRF/错误体解析）由 packages/api-client 自己的
 * 测试覆盖，两边不重复。
 */
const {
  listUsersMock,
  createUserMock,
  updateDisplayNameMock,
  updateUserStatusMock,
  resetTeacherPasswordMock,
} = vi.hoisted(() => ({
  listUsersMock: vi.fn(),
  createUserMock: vi.fn(),
  updateDisplayNameMock: vi.fn(),
  updateUserStatusMock: vi.fn(),
  resetTeacherPasswordMock: vi.fn(),
}))

vi.mock('../../lib/admin-users-api.ts', () => ({
  listUsers: listUsersMock,
  createUser: createUserMock,
  updateDisplayName: updateDisplayNameMock,
  updateUserStatus: updateUserStatusMock,
  resetTeacherPassword: resetTeacherPasswordMock,
  getUser: vi.fn(),
}))

/** 手写 deferred：用来制造"后发的请求先返回"的并发场景。 */
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => undefined
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

const ADMIN = makeAdminUser()
const TEACHER = makeTeacher({ lastLoginAt: '2026-02-01T09:30:00Z' })

describe('用户列表 store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    listUsersMock.mockReset().mockResolvedValue(makeUserListResponse([ADMIN, TEACHER]))
    createUserMock.mockReset().mockResolvedValue(makeStudent())
    updateDisplayNameMock.mockReset().mockResolvedValue(null)
    updateUserStatusMock.mockReset().mockResolvedValue(null)
    resetTeacherPasswordMock.mockReset()
  })

  it('初始状态：未加载、无数据、无错误', () => {
    const store = useUsersStore()

    expect(store.items).toEqual([])
    expect(store.total).toBe(0)
    expect(store.page).toBe(1)
    expect(store.hasLoaded).toBe(false)
    expect(store.isEmpty).toBe(false)
    expect(store.error).toBeNull()
  })

  it('加载成功：写入列表与总数，hasLoaded 置真，loading 归位', async () => {
    const store = useUsersStore()

    const pending = store.fetchList()
    expect(store.loading).toBe(true)
    await pending

    expect(store.loading).toBe(false)
    expect(store.hasLoaded).toBe(true)
    expect(store.items.map((user) => user.account)).toEqual(['admin', 'teacher001'])
    expect(store.total).toBe(2)
    // 默认查询：第 1 页、每页 50，且不带任何筛选参数。
    expect(listUsersMock).toHaveBeenCalledWith(
      { page: 1, pageSize: 50, q: undefined, role: undefined, status: undefined },
      { signal: expect.any(AbortSignal) },
    )
  })

  it('加载失败：记录 ApiError，但绝不把失败当成"列表为空"', async () => {
    listUsersMock.mockRejectedValue(new ApiError({ code: 'INTERNAL', status: 500 }))
    const store = useUsersStore()

    await store.fetchList()

    expect(store.error?.code).toBe('INTERNAL')
    expect(store.loading).toBe(false)
    // hasLoaded 保持 false：失败的请求没有回答"列表是不是空的"，
    // 否则界面会显示"还没有任何账号"——用空状态撒谎。
    expect(store.hasLoaded).toBe(false)
    expect(store.isEmpty).toBe(false)
  })

  it('非 ApiError 的异常也归一成 INTERNAL，不把原始异常抛给界面', async () => {
    listUsersMock.mockRejectedValue(new TypeError('Failed to fetch'))
    const store = useUsersStore()

    await store.fetchList()

    expect(store.error?.code).toBe('INTERNAL')
  })

  it('筛选变化重置到第 1 页（否则用户会在一个空页面上以为"搜不到"）', async () => {
    const store = useUsersStore()
    await store.fetchList()
    store.setPage(3)

    expect(store.setFilters({ q: 'zha' })).toBe(true)
    expect(store.page).toBe(1)
    expect(store.q).toBe('zha')

    // 值没变时不算变化，也就不会触发多余请求。
    expect(store.setFilters({ q: 'zha' })).toBe(false)
  })

  it('applyQuery 忠实地应用 URL：筛选与页码都来自地址栏', async () => {
    listUsersMock.mockResolvedValue(makeUserListResponse([ADMIN], { page: 3 }))
    const store = useUsersStore()
    await store.fetchList()
    listUsersMock.mockClear()

    // 手改地址栏成 ?q=zha&role=STUDENT&page=3 是合法输入：URL 是列表视图的唯一真相，
    // 因此这里必须请求第 3 页，而不是自作主张回到第 1 页。
    await store.applyQuery({ q: 'zha', role: 'STUDENT', status: null, page: 3 })

    expect(listUsersMock).toHaveBeenCalledTimes(1)
    expect(listUsersMock.mock.calls[0]?.[0]).toMatchObject({
      q: 'zha',
      role: 'STUDENT',
      page: 3,
    })
    expect(store.page).toBe(3)
  })

  it('页码与每页条数以后端返回值为准（后端可能钳制越界页码）', async () => {
    listUsersMock.mockResolvedValue(makeUserListResponse([ADMIN], { page: 2, pageSize: 20 }))
    const store = useUsersStore()

    await store.applyQuery({ q: '', role: null, status: null, page: 99 })

    // 前端要是自己记一套页码，分页控件算出来的页数就会和真实结果不一致。
    expect(store.page).toBe(2)
    expect(store.pageSize).toBe(20)
  })

  it('applyQuery 在条件没变时不重复请求（前进/后退不该产生额外流量）', async () => {
    const store = useUsersStore()
    await store.applyQuery({ q: '', role: null, status: null, page: 1 })
    listUsersMock.mockClear()

    await store.applyQuery({ q: '', role: null, status: null, page: 1 })

    expect(listUsersMock).not.toHaveBeenCalled()
  })

  it('并发请求：慢的旧响应不会覆盖新结果', async () => {
    const first = deferred<AdminUserListResponse>()
    const second = deferred<AdminUserListResponse>()
    listUsersMock.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const store = useUsersStore()

    const firstCall = store.fetchList()
    const secondCall = store.fetchList()
    // 后发的请求先返回：它才代表用户当前看到的筛选条件。
    second.resolve(makeUserListResponse([makeStudent()]))
    await secondCall
    first.resolve(makeUserListResponse([makeTeacher(), makeStudent()]))
    await firstCall

    expect(store.items.map((user) => user.account)).toEqual(['S10086'])
  })

  it('创建成功后重新拉列表，并记下"刚创建的账号"用于提示', async () => {
    const store = useUsersStore()
    await store.fetchList()
    listUsersMock.mockClear()

    const created = await store.createUser({
      account: 'S10087',
      displayName: '李四',
      role: 'STUDENT',
    })

    expect(created.account).toBe('S10086') // 返回值来自后端（mock 夹具），不是本地拼的
    expect(createUserMock).toHaveBeenCalledWith({
      account: 'S10087',
      displayName: '李四',
      role: 'STUDENT',
    })
    // 创建后必须重新拉列表：新账号是否落在当前筛选条件里由后端决定，本地插入会骗人。
    expect(listUsersMock).toHaveBeenCalledTimes(1)
    expect(store.justCreatedAccount).toBe('S10087')

    store.clearCreatedNotice()
    expect(store.justCreatedAccount).toBeNull()
  })

  it('创建失败时不改本地列表，也不留"已创建"提示（错误交给调用方按码处理）', async () => {
    createUserMock.mockRejectedValue(new ApiError({ code: 'ACCOUNT_ALREADY_EXISTS', status: 409 }))
    const store = useUsersStore()
    await store.fetchList()
    listUsersMock.mockClear()

    await expect(
      store.createUser({ account: 'S10086', displayName: '张三', role: 'STUDENT' }),
    ).rejects.toBeInstanceOf(ApiError)

    expect(listUsersMock).not.toHaveBeenCalled()
    expect(store.justCreatedAccount).toBeNull()
    expect(store.mutating).toBe(false)
  })

  it('停用：先调用状态接口，再以后端为准刷新列表', async () => {
    const store = useUsersStore()
    await store.fetchList()
    listUsersMock.mockClear()

    await store.updateStatus('user-teacher-1', 'DISABLED')

    expect(updateUserStatusMock).toHaveBeenCalledWith('user-teacher-1', { status: 'DISABLED' })
    expect(listUsersMock).toHaveBeenCalledTimes(1)
    expect(store.mutating).toBe(false)
  })

  it('停用失败时不刷新列表（没有成功的写操作就没有新状态）', async () => {
    updateUserStatusMock.mockRejectedValue(new ApiError({ code: 'INVALID_REQUEST', status: 400 }))
    const store = useUsersStore()
    await store.fetchList()
    listUsersMock.mockClear()

    await expect(store.updateStatus('user-admin-1', 'DISABLED')).rejects.toBeInstanceOf(ApiError)

    expect(listUsersMock).not.toHaveBeenCalled()
    expect(store.mutating).toBe(false)
  })

  it('改显示名：后端返回 DTO 时只更新那一行，不重新拉整个列表', async () => {
    updateDisplayNameMock.mockResolvedValue(makeAdminUser({ displayName: '新名字' }))
    const store = useUsersStore()
    await store.fetchList()
    listUsersMock.mockClear()

    await store.updateDisplayName('user-admin-1', '新名字')

    expect(store.items[0]?.displayName).toBe('新名字')
    expect(store.items[1]?.displayName).toBe('李老师')
    expect(listUsersMock).not.toHaveBeenCalled()
  })

  it('改显示名：响应没有 DTO（204）时重新拉列表，而不是假装本地改成功', async () => {
    updateDisplayNameMock.mockResolvedValue(null)
    const store = useUsersStore()
    await store.fetchList()
    listUsersMock.mockClear()

    await store.updateDisplayName('user-admin-1', '新名字')

    expect(listUsersMock).toHaveBeenCalledTimes(1)
    // 列表内容仍然来自后端（mock 里 displayName 未变），本地没有留下半成品状态。
    expect(store.items[0]?.displayName).toBe('系统管理员')
  })

  it('重置老师密码：把一次性明文原样返回，不写进任何状态或 Web Storage', async () => {
    resetTeacherPasswordMock.mockResolvedValue({
      user: TEACHER,
      password: 'server-generated-password',
    })
    const store = useUsersStore()
    await store.fetchList()

    const response = await store.resetTeacherPassword('user-teacher-1')

    expect(resetTeacherPasswordMock).toHaveBeenCalledWith('user-teacher-1', {})
    expect(response.password).toBe('server-generated-password')
    // 明文密码绝不进入 store（§41）：整个 store 的可枚举状态里都不该出现它。
    expect(JSON.stringify(store.$state)).not.toContain('server-generated-password')
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })

  it('重置老师密码：管理员指定密码时把它放进请求体，响应里没有明文', async () => {
    resetTeacherPasswordMock.mockResolvedValue({ user: TEACHER })
    const store = useUsersStore()

    const response = await store.resetTeacherPassword('user-teacher-1', 'chosen-password-1')

    expect(resetTeacherPasswordMock).toHaveBeenCalledWith('user-teacher-1', {
      password: 'chosen-password-1',
    })
    expect(response.password).toBeUndefined()
    expect(JSON.stringify(store.$state)).not.toContain('chosen-password-1')
  })
})
