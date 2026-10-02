import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useSessionStore } from '../stores/session'
import DashboardView from '../views/DashboardView.vue'
import LoginView from '../views/LoginView.vue'
import { createAuthGuard, type AuthGuardConfig } from './guard'
// 引入 meta 的类型增强（requiresAuth / title 必填），见 route-meta.ts 的说明。
import './route-meta'
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
  {
    path: '/admin/login',
    name: 'admin-login',
    component: LoginView,
    meta: { requiresAuth: false, title: '管理员登录' },
  },

  // Phase 2：账号体系概览与用户管理入口。
  {
    path: '/admin/dashboard',
    name: 'admin-dashboard',
    component: DashboardView,
    meta: { requiresAuth: true, title: '管理概览' },
  },

  // Phase 2：账号列表（筛选、启用/禁用）。
  {
    path: '/admin/users',
    name: 'admin-users',
    component: UsersView,
    meta: { requiresAuth: true, title: '用户管理' },
  },

  // Phase 2：新建账号（学生 / 教师 / 管理员）。
  {
    path: '/admin/users/new',
    name: 'admin-user-new',
    component: UserNewView,
    meta: { requiresAuth: true, title: '新建账号' },
  },
]

/** 本 app 的守卫配置；三个 app 只有这张表不同，守卫逻辑由 guard.ts 统一实现。 */
export const authGuardConfig: AuthGuardConfig = {
  // 管理端权限最高（能创建、停用账号），会话串入口的后果也最严重：
  // 一旦 role 不是 ADMIN 就必须立刻撤销本入口会话并回到登录页，
  // 绝不允许渲染出一个"看着像后台"的空界面。
  role: 'ADMIN',
  loginRouteName: 'admin-login',
  loginPath: '/admin/login',
  homeRouteName: 'admin-dashboard',
  roleMismatchNotice: '当前账号不是管理员账号，已退出登录。请使用管理员账号登录。',
}

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // 页面间跳转回到顶部：长列表进出时不保留旧滚动位置。
  scrollBehavior: () => ({ top: 0 }),
})

/**
 * 装上 Phase 1 守卫：加载会话 → 校验角色 → 重定向。
 *
 * 这里传的是"取会话 store 的函数"而不是 store 实例：本模块在 main.ts 里被 import
 * 时 pinia 还没安装，而守卫真正执行是在首次导航（那时 pinia 已就绪）。
 *
 * 守卫**不**阻拦"会话未确认（网络失败）"的导航，见 guard.ts 末尾的说明；
 * 真正的授权边界始终在后端（§37 / §63）。
 */
router.beforeEach(createAuthGuard(authGuardConfig, () => useSessionStore()))

// 浏览器标签页标题跟着路由走：老师常常同时开着教师端与学生端联调，
// 三个入口必须在标签栏上就能区分。
router.afterEach((to) => {
  const title = to.meta.title
  if (title) document.title = `${title} · ClassWatch 管理端`
})
