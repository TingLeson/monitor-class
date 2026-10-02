import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useSessionStore } from '../stores/session'
import ClassroomDetailView from '../views/ClassroomDetailView.vue'
import ClassroomMonitorView from '../views/ClassroomMonitorView.vue'
import ClassroomNewView from '../views/ClassroomNewView.vue'
import ClassroomsView from '../views/ClassroomsView.vue'
import DashboardView from '../views/DashboardView.vue'
import LoginView from '../views/LoginView.vue'
import { createAuthGuard, type AuthGuardConfig } from './guard'
// 引入 meta 的类型增强（requiresAuth / title 必填），见 route-meta.ts 的说明。
import './route-meta'

/**
 * 教师端路由表（§55「页面规划 · Teacher」）。
 *
 * WHY 路径自带 /teacher 前缀（而不是 basename）：
 * 生产环境三个入口是三个独立域名（student./teacher./admin.），前缀让日志、nginx
 * location、后端 redirect 白名单都能一眼区分来源；同时 `createWebHistory()` 不带
 * basename，页面显示的 path 与实际 URL 完全一致，不存在"少了前缀导致 404"的错位。
 *
 * 这份 routes 数组单独导出，供路由测试逐条钉死 §55 的路径契约。
 */
export const routes: RouteRecordRaw[] = [
  // 根路径进登录页：教师端没有公开首页。
  { path: '/', redirect: '/teacher/login' },

  // Phase 1：密码登录（§39）。
  {
    path: '/teacher/login',
    name: 'teacher-login',
    component: LoginView,
    meta: { requiresAuth: false, title: '教师登录' },
  },

  // Phase 3：工作台概览（开启/关闭课堂的入口）。
  {
    path: '/teacher/dashboard',
    name: 'teacher-dashboard',
    component: DashboardView,
    meta: { requiresAuth: true, title: '工作台' },
  },

  // Phase 3：课堂列表。
  {
    path: '/teacher/classrooms',
    name: 'teacher-classrooms',
    component: ClassroomsView,
    meta: { requiresAuth: true, title: '我的课堂' },
  },

  // Phase 3：新建课堂。
  // WHY 必须排在 '/teacher/classrooms/:id' 之前：静态段虽然优先级更高，
  // 但显式顺序能让"new 被当成课堂 id"这类事故在阅读时就排除掉。
  {
    path: '/teacher/classrooms/new',
    name: 'teacher-classroom-new',
    component: ClassroomNewView,
    meta: { requiresAuth: true, title: '新建课堂' },
  },

  // Phase 3：课堂详情（改名、授权学生名单、开启/关闭）。
  {
    path: '/teacher/classrooms/:id',
    name: 'teacher-classroom-detail',
    component: ClassroomDetailView,
    meta: { requiresAuth: true, title: '课堂详情' },
  },

  // Phase 7：老师监督墙（多学生桌面网格 + Focus View，§29 / §30）。
  {
    path: '/teacher/classrooms/:id/monitor',
    name: 'teacher-classroom-monitor',
    component: ClassroomMonitorView,
    meta: { requiresAuth: true, title: '课堂监督墙' },
  },
]

/** 本 app 的守卫配置；三个 app 只有这张表不同，守卫逻辑由 guard.ts 统一实现。 */
export const authGuardConfig: AuthGuardConfig = {
  // GET /auth/me 走的就是本入口的会话，因此 role 理论上必然匹配。保留这项校验，
  // 是为了让"会话串了入口"或"后端角色判定回归"立刻表现为"回登录页 + 明确提示"，
  // 而不是渲染出一个所有请求都 403 的空数据界面。
  role: 'TEACHER',
  loginRouteName: 'teacher-login',
  loginPath: '/teacher/login',
  homeRouteName: 'teacher-dashboard',
  roleMismatchNotice: '当前账号不是教师账号，已退出登录。请使用教师账号登录。',
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
  if (title) document.title = `${title} · ClassWatch 教师端`
})
