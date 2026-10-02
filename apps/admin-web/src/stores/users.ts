import { ApiError, isApiError } from '@classwatch/api-client'
import {
  ADMIN_USER_PAGE_SIZE_DEFAULT,
  ADMIN_USER_PAGE_SIZE_MAX,
  type AdminUser,
  type AdminUserListQuery,
  type CreateUserRequest,
  type ResetTeacherPasswordResponse,
  type Role,
  type UserStatus,
} from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  createUser as createUserRequest,
  listUsers,
  resetTeacherPassword as resetTeacherPasswordRequest,
  updateDisplayName as updateDisplayNameRequest,
  updateUserStatus as updateUserStatusRequest,
} from '../lib/admin-users-api.ts'
import type { UserListQueryState } from '../lib/user-list-query.ts'

/**
 * 管理端用户列表状态（§4 / §68）。
 *
 * 三条贯穿全文件的原则：
 *
 * 1. **不做乐观更新**。所有写操作先等后端返回，成功之后才动本地状态；
 *    停用与创建直接重新拉列表（列表排序是 `created_at DESC`，本地插入的顺序
 *    与后端不一致，而"账号是不是真的被停用了"只有后端知道）。
 * 2. **列表错误留在 store，表单错误留给调用方**。`fetchList` 的失败要显示在列表区域，
 *    因此记进 `error`；`createUser` / `updateStatus` 这类操作的失败必须由调用方按
 *    错误码决定挂到哪个字段，所以**原样抛出**，不在这里吞掉。
 * 3. **一次性密码不落在 store**。`resetTeacherPassword` 把服务端生成的明文密码
 *    直接返回给调用方展示一次，不写进任何响应式状态、不写 Web Storage（§38 / §41）。
 */
export const useUsersStore = defineStore('admin-users', () => {
  const items = ref<AdminUser[]>([])
  const total = ref(0)
  const page = ref(1)
  const pageSize = ref(ADMIN_USER_PAGE_SIZE_DEFAULT)
  const q = ref('')
  const role = ref<Role | null>(null)
  const status = ref<UserStatus | null>(null)

  /** 列表请求进行中（首次加载显示骨架，已有数据时只显示"正在更新"）。 */
  const loading = ref(false)
  /** 列表请求失败；保存 ApiError 而不是字符串，文案映射交给视图（错误码是契约）。 */
  const error = ref<ApiError | null>(null)
  /**
   * 是否成功加载过至少一次。
   * WHY 与 `items.length` 分开：加载失败时 `items` 也是空的，但那时**不能**显示
   * "还没有任何账号"——那是在用空状态撒谎。空状态只在 hasLoaded 为真时才有意义。
   */
  const hasLoaded = ref(false)
  /** 写操作进行中（创建 / 改名 / 改状态 / 重置密码），用于禁用按钮防重复提交。 */
  const mutating = ref(false)
  /**
   * 刚刚创建成功的账号（新建页跳回列表后用来提示并高亮那一行）。
   * 只活在内存里：它是一次性确认信息，刷新页面后没有保留价值。
   */
  const justCreatedAccount = ref<string | null>(null)

  /**
   * 请求序号。列表请求会并发（防抖搜索、快速翻页），后发出的请求结果必须覆盖先发出的，
   * 否则慢的旧响应会把新筛选的结果盖掉——用户看到的是"搜了但列表没变"。
   */
  let requestSeq = 0
  let inFlightController: AbortController | null = null

  const isEmpty = computed(() => hasLoaded.value && items.value.length === 0)
  /** 当前筛选条件（视图用它构造 URL；page 也在这里，翻页与筛选共用同一份真相）。 */
  const query = computed<UserListQueryState>(() => ({
    q: q.value,
    role: role.value,
    status: status.value,
    page: page.value,
  }))

  function buildRequestQuery(): AdminUserListQuery {
    return {
      q: q.value || undefined,
      role: role.value ?? undefined,
      status: status.value ?? undefined,
      page: page.value,
      // 上限由后端契约给定（200）；前端钳制只是避免发出注定被拒的请求。
      pageSize: Math.min(pageSize.value, ADMIN_USER_PAGE_SIZE_MAX),
    }
  }

  async function fetchList(): Promise<void> {
    const seq = ++requestSeq
    // 取消上一次仍在飞行中的请求：搜索框每次防抖都会发新请求，不取消就是让多个
    // 响应互相竞争（seq 已经兜底正确性，这里只是不浪费连接与后端算力）。
    inFlightController?.abort()
    const controller = new AbortController()
    inFlightController = controller

    loading.value = true
    error.value = null
    try {
      const response = await listUsers(buildRequestQuery(), { signal: controller.signal })
      if (seq !== requestSeq) return
      items.value = response.users
      total.value = response.total
      // 页码与每页条数以**后端返回值**为准：后端可能钳制过 pageSize，
      // 前端要是自己记一套，分页控件算出来的页数就会和真实结果不一致。
      page.value = response.page
      pageSize.value = response.pageSize
      hasLoaded.value = true
    } catch (cause) {
      if (seq !== requestSeq) return
      error.value = isApiError(cause) ? cause : new ApiError({ code: 'INTERNAL', cause })
      // 刻意不动 hasLoaded：失败的请求没有回答"列表是不是空的"。
    } finally {
      // 只有最新一次请求才有资格结束 loading，否则旧请求会把新请求的加载态提前关掉。
      if (seq === requestSeq) loading.value = false
    }
  }

  /**
   * 修改筛选条件，返回是否真的变了。
   *
   * 筛选一变就回到第 1 页：停在第 3 页再搜一个只有 2 条结果的词，用户会看到
   * 一个空列表并以为"搜不到"，而其实数据在第 1 页。
   */
  function setFilters(patch: {
    q?: string
    role?: Role | null
    status?: UserStatus | null
  }): boolean {
    let changed = false
    if (patch.q !== undefined && patch.q !== q.value) {
      q.value = patch.q
      changed = true
    }
    if (patch.role !== undefined && patch.role !== role.value) {
      role.value = patch.role
      changed = true
    }
    if (patch.status !== undefined && patch.status !== status.value) {
      status.value = patch.status
      changed = true
    }
    if (changed) page.value = 1
    return changed
  }

  /** 设置页码；非法值（0、负数、NaN）一律落到第 1 页而不是原样存下来。 */
  function setPage(next: number): boolean {
    const clamped = Number.isFinite(next) ? Math.max(1, Math.floor(next)) : 1
    if (clamped === page.value) return false
    page.value = clamped
    return true
  }

  /**
   * 应用 URL query（视图 → store 的唯一入口）。
   *
   * 只有在"条件确实变了"或"还没成功加载过"时才发请求：浏览器前进/后退、
   * 或一次无效导航（同一个查询串）不该产生重复请求。
   */
  async function applyQuery(next: UserListQueryState): Promise<void> {
    const filtersChanged = setFilters({ q: next.q, role: next.role, status: next.status })
    const pageChanged = setPage(next.page)
    if (filtersChanged || pageChanged || !hasLoaded.value) await fetchList()
  }

  /** 用后端返回的 DTO 替换本地同一行（只有写接口明确返回了新 DTO 时才用）。 */
  function replaceItem(updated: AdminUser): void {
    items.value = items.value.map((item) => (item.id === updated.id ? updated : item))
  }

  /**
   * 创建账号（老师 / 学生）。
   *
   * 成功后重新拉列表而不是本地插入：列表按 `created_at DESC` 排序，而新账号是否
   * 落在**当前筛选条件**里由后端判断（例如筛选"学生"时创建一个老师，它本就不该出现）。
   */
  async function createUser(input: CreateUserRequest): Promise<AdminUser> {
    mutating.value = true
    try {
      const created = await createUserRequest(input)
      justCreatedAccount.value = input.account
      await fetchList()
      return created
    } finally {
      mutating.value = false
    }
  }

  /**
   * 修改显示名。后端明确返回新 DTO 时只更新那一行（列表顺序不会因为改名而变），
   * 返回 204 / 空体时才重新拉列表——不猜、也不假装本地改成功。
   */
  async function updateDisplayName(id: string, displayName: string): Promise<void> {
    mutating.value = true
    try {
      const updated = await updateDisplayNameRequest(id, { displayName })
      if (updated) replaceItem(updated)
      else await fetchList()
    } finally {
      mutating.value = false
    }
  }

  /**
   * 启用 / 停用。停用会**立即撤销该账号全部会话**（§41），因此无论后端是否返回 DTO
   * 都重新拉列表：管理员看到的必须是后端的真实状态，而不是自己刚点下去的那个值。
   */
  async function updateStatus(id: string, nextStatus: UserStatus): Promise<void> {
    mutating.value = true
    try {
      await updateUserStatusRequest(id, { status: nextStatus })
      await fetchList()
    } finally {
      mutating.value = false
    }
  }

  /**
   * 重置老师密码。
   *
   * 不传 password 时由服务端生成，明文只在这一个响应里出现；本函数把它原样交给
   * 调用方去"只展示一次"，绝不写入 items / 任何 ref / Web Storage（§41 / §63）。
   */
  async function resetTeacherPassword(
    id: string,
    password?: string,
  ): Promise<ResetTeacherPasswordResponse> {
    mutating.value = true
    try {
      const response = await resetTeacherPasswordRequest(id, password ? { password } : {})
      // 重置会撤销该老师全部会话并刷新 updatedAt；用返回的 DTO 同步那一行。
      replaceItem(response.user)
      return response
    } finally {
      mutating.value = false
    }
  }

  /** 消费掉"刚创建"的提示（用户关闭提示或改变筛选条件时调用）。 */
  function clearCreatedNotice(): void {
    justCreatedAccount.value = null
  }

  return {
    items,
    total,
    page,
    pageSize,
    q,
    role,
    status,
    loading,
    error,
    hasLoaded,
    mutating,
    justCreatedAccount,
    isEmpty,
    query,
    fetchList,
    setFilters,
    setPage,
    applyQuery,
    createUser,
    updateDisplayName,
    updateStatus,
    resetTeacherPassword,
    clearCreatedNotice,
  }
})
