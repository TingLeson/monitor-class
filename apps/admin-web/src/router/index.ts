import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import DashboardView from '../views/DashboardView.vue'
import LoginView from '../views/LoginView.vue'
import UserNewView from '../views/UserNewView.vue'
import UsersView from '../views/UsersView.vue'

/**
 * 管理端路由表（§55「页面规划 · Admin」）。
 *
 * WHY 路径自带 /admin 前缀（而不是 basename）：
 * 生产环境三个入口是三个独立域名（student./teacher./admin.），前缀让日志、nginx
 * location、后端 redirect 白名单都能一眼区分来源；同时 `createWebHistory()` 不带
 * basename，页面显示的 path 与实际 URL 完全一致，不存在"少了前缀导致 404"的错位。
 *
 * 这份 routes 数组单独导出，供路由测试逐条钉死 §55 的路径契约。
 */
export const routes: RouteRecordRaw[] = [
  // 根路径进登录页：管理端没有公开首页。
  { path: '/', redirect: '/admin/login' },

  // Phase 1：管理员密码登录（§40）。
  { path: '/admin/login', name: 'admin-login', component: LoginView },

  // Phase 2：账号体系概览与用户管理入口。
  { path: '/admin/dashboard', name: 'admin-dashboard', component: DashboardView },

  // Phase 2：账号列表（筛选、启用/禁用）。
  { path: '/admin/users', name: 'admin-users', component: UsersView },

  // Phase 2：新建账号（学生 / 教师 / 管理员）。
  { path: '/admin/users/new', name: 'admin-user-new', component: UserNewView },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // 页面间跳转回到顶部：长列表（用户管理）进出时不保留旧滚动位置。
  scrollBehavior: () => ({ top: 0 }),
})

// ===========================================================================
// Phase 1 的导航守卫插入点：就在本注释下方（router 创建之后）。
//
// 守卫顺序固定为「加载会话 → 校验角色 → 重定向」，三步缺一不可：
//
// 1) 加载会话：await useSessionStore().loadSession()（Phase 1 实现），
//    即 GET /api/v1/admin/auth/me；HttpOnly Cookie 由浏览器自动携带（§41）。
//    WHY 必须先加载：刷新页面时 pinia 是空的，若直接判断 isAuthenticated，
//    已登录管理员会立刻被踢回登录页。
//
// 2) 校验角色：user.role 必须是 'ADMIN'。
//    WHY 不跳转到教师端/学生端：三个入口物理分离（§5），跨 app 跳转会让会话与
//    CORS allowlist 混乱；正确做法是清空本地会话并停留在本 app 的登录页。
//    已经过期的管理员会话必须表现为"必须重新登录"，不允许静默降级成其他角色视图。
//
// 3) 重定向：未登录 → /admin/login；已登录访问 /admin/login → /admin/dashboard。
//
// WHY 前端守卫**不是**授权边界（§37 / §63）：
// 管理端能创建账号、停用账号、重置教师密码，是最敏感的入口。前端代码完全在用户
// 控制之下（DevTools、改包、直接 curl 都能绕过守卫），所以后端必须对每个
// /api/v1/admin/* 请求独立执行 Session Middleware → Load User → ACTIVE → RBAC，
// 绝不允许出现"只要页面没渲染按钮就安全"的假设。
// ===========================================================================
