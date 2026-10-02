import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import ClassroomDetailView from '../views/ClassroomDetailView.vue'
import ClassroomsView from '../views/ClassroomsView.vue'
import LoginView from '../views/LoginView.vue'
import SessionView from '../views/SessionView.vue'

/**
 * 学生端路由表（§55「页面规划 · Student」）。
 *
 * WHY 路径自带 /student 前缀（而不是 basename）：
 * 生产环境三个入口是三个独立域名（student./teacher./admin.），前缀让日志、nginx
 * location、后端 redirect 白名单都能一眼区分来源；同时 `createWebHistory()` 不带
 * basename，页面显示的 path 与实际 URL 完全一致，不存在"少了前缀导致 404"的错位。
 *
 * 这份 routes 数组单独导出，供路由测试逐条钉死 §55 的路径契约。
 */
export const routes: RouteRecordRaw[] = [
  // 根路径进登录页：学生端没有公开首页，未登录时唯一有意义的落点就是登录。
  { path: '/', redirect: '/student/login' },

  // Phase 1：账号登录（学生无密码，§38）。
  { path: '/student/login', name: 'student-login', component: LoginView },

  // Phase 4：我的课堂列表，只包含被 ClassroomStudent 授权的课堂（§14）。
  { path: '/student/classrooms', name: 'student-classrooms', component: ClassroomsView },

  // Phase 4/5：课堂说明 + 隐私提示 + 整个屏幕 Gate 的落地页（§15 Step 2）。
  // :id 是课堂 UUID，组件通过 useRoute() 读取（不使用 props: true，避免未声明的
  // 路由参数以 HTML 属性形式透传到根元素上）。
  {
    path: '/student/classrooms/:id',
    name: 'student-classroom-detail',
    component: ClassroomDetailView,
  },

  // Phase 6：课堂内状态页（屏幕/摄像头/麦克风/网络，§56）。
  {
    path: '/student/session/:sessionId',
    name: 'student-session',
    component: SessionView,
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // 页面间跳转回到顶部：长列表（我的课堂）进出时不保留旧滚动位置。
  scrollBehavior: () => ({ top: 0 }),
})

// ===========================================================================
// Phase 1 的导航守卫插入点：就在本注释下方（router 创建之后、模块被 import 之前）。
//
// 守卫顺序固定为「加载会话 → 校验角色 → 重定向」，三步缺一不可：
//
// 1) 加载会话：await useSessionStore().loadSession()（Phase 1 实现），
//    即 GET /api/v1/student/auth/me；HttpOnly Cookie 由浏览器自动携带（§38/§41）。
//    WHY 必须先加载：刷新页面时 pinia 是空的，若直接判断 isAuthenticated，
//    已登录学生会立刻被踢回登录页。
//
// 2) 校验角色：user.role 必须是 'STUDENT'。
//    WHY 不跳转到教师端/管理端：三个入口物理分离（§5），跨 app 跳转会让会话与
//    CORS allowlist 混乱；正确做法是清空本地会话并停留在本 app 的登录页。
//
// 3) 重定向：未登录访问受保护页面 → /student/login；
//    已登录访问 /student/login → /student/classrooms。
//
// WHY 前端守卫**不是**授权边界（§37 / §63）：
// 守卫只能少发一次注定 403 的请求，属于 UX 优化。前端代码完全在用户控制之下
// （DevTools、改包、直接 curl 接口都能绕过），所以后端仍必须对每个请求独立执行
// Session Middleware → Load User → ACTIVE → RBAC → Resource Ownership，
// 任何"前端挡住了就安全"的假设都会直接变成越权漏洞。
// ===========================================================================
