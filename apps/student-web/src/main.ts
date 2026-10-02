import { createPinia } from 'pinia'
import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import './style.css'

/**
 * 学生端入口。
 *
 * 顺序要求：先装 pinia 再装 router。
 * Phase 1 的导航守卫要在路由解析前读取会话 store，如果 pinia 还没注册，
 * 守卫里 `useSessionStore()` 会直接抛错（Pinia 未安装）。
 */
createApp(App).use(createPinia()).use(router).mount('#app')
