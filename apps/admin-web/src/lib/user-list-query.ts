import { type Role, type UserStatus, isRole, isUserStatus } from '@classwatch/shared-types'
import type { LocationQuery, LocationQueryRaw } from 'vue-router'

/**
 * 账号列表的筛选条件 ↔ URL query（docs/frontend/admin.md §2.1）。
 *
 * WHY 把筛选条件放进 URL：刷新、分享链接、浏览器前进/后退都必须保持视图。
 * 这也是"搜索/筛选状态放哪"的答案——不放 store 之外的第二个地方，URL 是入口，
 * store 承接执行，两者由 UsersView 单向同步。
 *
 * 解析规则只有一条：**非法值一律当作未筛选**。URL 是外部输入（用户可以手改，
 * 旧链接可能来自更早的版本），既不能信任，也不要把清洗后的值写回地址栏——
 * 那会把用户的一次笔误永久固化在 URL 里。
 *
 * 这条规则同时保护请求：后端对列表 query 是**严格**的（未知参数、非法枚举、
 * `?page=abc` 一律 400，不做兜底），所以非法值必须在进入 store 之前就被丢掉，
 * 否则一次手改地址栏就会变成"账号列表加载失败"。
 */

/** 解析后的筛选条件：字段都已校验过，可直接交给 store 与接口。 */
export interface UserListQueryState {
  q: string
  role: Role | null
  status: UserStatus | null
  page: number
}

/**
 * 取 query 里的第一个字符串值。
 *
 * `?q=a&q=b` 会解析成数组。取第一个而不是拒绝整条链接：重复参数通常来自手工拼接，
 * 让页面照常工作、忽略多余的那份，比甩一个错误页更有用。
 */
function firstValue(value: unknown): string | null {
  if (typeof value === 'string') return value
  if (Array.isArray(value) && typeof value[0] === 'string') return value[0]
  return null
}

/** 页码：非整数或小于 1 一律回到第 1 页（`?page=abc`、`?page=-3`、`?page=1.5`）。 */
function parsePage(raw: string | null): number {
  if (raw === null) return 1
  const parsed = Number.parseInt(raw, 10)
  if (!Number.isInteger(parsed) || parsed < 1) return 1
  return parsed
}

/**
 * 解析任何"query 形状"的输入。
 *
 * 参数同时接受 `LocationQuery`（vue-router 解析出来的，值只可能是 string / null / 数组）
 * 与 `LocationQueryRaw`（我们自己构造、准备写回地址栏的那份，值里可能有数字）。
 * 两者都只是"键值对"，而非法值一律按未筛选处理，因此不需要为它们各写一份实现。
 */
export function parseUserListQuery(query: LocationQuery | LocationQueryRaw): UserListQueryState {
  const rawRole = firstValue(query.role)
  const rawStatus = firstValue(query.status)

  return {
    q: (firstValue(query.q) ?? '').trim(),
    role: isRole(rawRole) ? rawRole : null,
    status: isUserStatus(rawStatus) ? rawStatus : null,
    page: parsePage(firstValue(query.page)),
  }
}

/**
 * 把筛选条件写回 URL：只写非默认值，默认状态就是干净的 `/admin/users`。
 *
 * WHY 不写 pageSize：V1 界面上没有"每页多少条"的控件，把它放进 URL 只会制造
 * 一个用户改不动、后端还可能拒绝的参数。分页大小由 store 的默认值决定。
 */
export function buildUserListQuery(state: UserListQueryState): LocationQueryRaw {
  const query: LocationQueryRaw = {}
  if (state.q) query.q = state.q
  if (state.role) query.role = state.role
  if (state.status) query.status = state.status
  if (state.page > 1) query.page = String(state.page)
  return query
}
