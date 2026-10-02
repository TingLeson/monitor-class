import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import ClassroomDetailView from '../views/ClassroomDetailView.vue'
import ClassroomMonitorView from '../views/ClassroomMonitorView.vue'
import ClassroomNewView from '../views/ClassroomNewView.vue'
import ClassroomsView from '../views/ClassroomsView.vue'
import DashboardView from '../views/DashboardView.vue'
import LoginView from '../views/LoginView.vue'

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
  { path: '/teacher/login', name: 'teacher-login', component: LoginView },

  // Phase 3：工作台概览（开启/关闭课堂的入口）。
  { path: '/teacher/dashboard', name: 'teacher-dashboard', component: DashboardView },

  // Phase 3：课堂列表。
  { path: '/teacher/classrooms', name: 'teacher-classrooms', component: ClassroomsView },

  // Phase 3：新建课堂。
  // WHY 必须排在 '/teacher/classrooms/:id' 之前：静态段虽然优先级更高，
  // 但显式顺序能让"new 被当成课堂 id"这类事故在阅读时就排除掉。
  {
    path: '/teacher/classrooms/new',
    name: 'teacher-classroom-new',
    component: ClassroomNewView,
  },

  // Phase 3：课堂详情（改名、授权学生名单、开启/关闭）。
  {
    path: '/teacher/classrooms/:id',
    name: 'teacher-classroom-detail',
    component: ClassroomDetailView,
  },

  // Phase 7：老师监督台（学生桌面网格，§29）。
  {
    path: '/teacher/classrooms/:id/monitor',
    name: 'teacher-classroom-monitor',
    component: ClassroomMonitorView,
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // 页面间跳转回到顶部：长列表（课堂、监督网格）进出时不保留旧滚动位置。
  scrollBehavior: () => ({ top: 0 }),
})

// ===========================================================================
// Phase 1 的导航守卫插入点：就在本注释下方（router 创建之后）。
//
// 守卫顺序固定为「加载会话 → 校验角色 → 重定向」，三步缺一不可：
//
// 1) 加载会话：await useSessionStore().loadSession()（Phase 1 实现），
//    即 GET /api/v1/teacher/auth/me；HttpOnly Cookie 由浏览器自动携带（§41）。
//    WHY 必须先加载：刷新页面时 pinia 是空的，若直接判断 isAuthenticated，
//    已登录老师会立刻被踢回登录页。
//
// 2) 校验角色：user.role 必须是 'TEACHER'。
//    WHY 不跳转到学生端/管理端：三个入口物理分离（§5），跨 app 跳转会让会话与
//    CORS allowlist 混乱；正确做法是清空本地会话并停留在本 app 的登录页。
//
// 3) 重定向：未登录 → /teacher/login；已登录访问 /teacher/login → /teacher/dashboard。
//
// WHY 前端守卫**不是**授权边界（§37 / §63）：
// 守卫只能少发一次注定 403 的请求，属于 UX 优化。前端代码完全在用户控制之下
// （DevTools、改包、直接 curl 接口都能绕过），所以后端仍必须对每个请求独立执行
// Session Middleware → Load User → ACTIVE → RBAC → Resource Ownership；
// 尤其是课堂的 open / close / monitor，必须校验 owner_teacher_id（§48/§49），
// 绝不能因为"前端只显示了自己的课堂"就跳过 ownership 检查。
// ===========================================================================
