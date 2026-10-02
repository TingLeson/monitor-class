import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useSessionStore } from '../stores/session'
import ClassroomDetailView from '../views/ClassroomDetailView.vue'
import ClassroomsView from '../views/ClassroomsView.vue'
import LoginView from '../views/LoginView.vue'
import SessionView from '../views/SessionView.vue'
import { createAuthGuard, type AuthGuardConfig } from './guard'
// 引入 meta 的类型增强（requiresAuth / title 必填），见 route-meta.ts 的说明。
import './route-meta'

/**
 * 学生端路由表（§55「页面规划 · Student」）。
 *
 * WHY 路径自带 /student 前缀（而不是 basename）：
 * 生产环境三个入口是三个独立域名（student./teacher./admin.），前缀让日志、nginx
 * location、后端 redirect 白名单都能一眼区分来源；同时 `createWebHistory()` 不带
 * basename，页面显示的 path 与实际 URL 完全一致，不存在"少了前缀导致 404"的错位。
 *
 * 每条路由都必须声明 meta（见 ./route-meta.ts 的类型约束）：
 * - requiresAuth：false 只有登录页，其余全部 true；
 * - title：浏览器标签页标题。
 *
 * 这份 routes 数组单独导出，供路由测试逐条钉死 §55 的路径契约。
 */
export const routes: RouteRecordRaw[] = [
  // 根路径进登录页：学生端没有公开首页，未登录时唯一有意义的落点就是登录。
  { path: '/', redirect: '/student/login' },

  // Phase 1：账号登录（学生无密码，§38）。
  {
    path: '/student/login',
    name: 'student-login',
    component: LoginView,
    meta: { requiresAuth: false, title: '学生登录' },
  },

  // Phase 4：我的课堂列表，只包含被 ClassroomStudent 授权的课堂（§14）。
  {
    path: '/student/classrooms',
    name: 'student-classrooms',
    component: ClassroomsView,
    meta: { requiresAuth: true, title: '我的课堂' },
  },

  // Phase 4/5：课堂说明 + 隐私提示 + 整个屏幕 Gate 的落地页（§15 Step 2）。
  // :id 是课堂 UUID，组件通过 useRoute() 读取（不使用 props: true，避免未声明的
  // 路由参数以 HTML 属性形式透传到根元素上）。
  {
    path: '/student/classrooms/:id',
    name: 'student-classroom-detail',
    component: ClassroomDetailView,
    meta: { requiresAuth: true, title: '进入课堂' },
  },

  // Phase 6：课堂内状态页（屏幕/摄像头/麦克风/网络，§56）。
  {
    path: '/student/session/:sessionId',
    name: 'student-session',
    component: SessionView,
    meta: { requiresAuth: true, title: '课堂中' },
  },
]

/** 本 app 的守卫配置；三个 app 只有这张表不同，守卫逻辑由 guard.ts 统一实现。 */
export const authGuardConfig: AuthGuardConfig = {
  // GET /auth/me 走的就是本入口的会话，因此 role 理论上必然匹配。保留这项校验，
  // 是为了让"会话串了入口"或"后端角色判定回归"立刻表现为"回登录页 + 明确提示"，
  // 而不是渲染出一个所有请求都 403 的空数据界面。
  role: 'STUDENT',
  loginRouteName: 'student-login',
  loginPath: '/student/login',
  homeRouteName: 'student-classrooms',
  roleMismatchNotice: '当前账号不是学生账号，已退出登录。请使用学生账号登录。',
}

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // 页面间跳转回到顶部：长列表（我的课堂）进出时不保留旧滚动位置。
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
  if (title) document.title = `${title} · ClassWatch 学生端`
})
