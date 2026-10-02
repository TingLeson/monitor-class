import 'vue-router'

/**
 * 路由 meta 的契约（Phase 1，§55）。
 *
 * WHY 用模块增强把这两项变成**必填**：`requiresAuth` 漏写就等于"这一页改成公开
 * 页面"，而且不会有任何编译错误——这是最容易悄悄发生的鉴权回归。声明成必填后，
 * 任何新增路由忘了写 meta 都会在 `pnpm -r typecheck` 阶段直接失败。
 */
declare module 'vue-router' {
  interface RouteMeta {
    /** 是否要求已登录。只有登录页是 false，其余一律 true。 */
    requiresAuth: boolean
    /** 页面标题（浏览器标签页 + 后续面包屑）。 */
    title: string
  }
}

export {}
