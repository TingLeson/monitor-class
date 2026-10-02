<script setup lang="ts">
import { AppShell, ProtectedRouteGate } from '@classwatch/ui'
import { computed, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { useSessionStore } from './stores/session'

/**
 * 教师端根组件：只负责"外壳 + 路由出口 + 顶栏账号区"。
 *
 * WHY 这里没有任何角色判断：教师端是一个物理独立的 SPA（§5），
 * 它的进程里不存在其他入口的页面代码，渲染的只有后端返回的 displayName。
 */
const session = useSessionStore()
const route = useRoute()
const router = useRouter()
const logoutPending = ref(false)

/**
 * 只在"需要登录、但会话尚未确认"时显示加载态。
 *
 * WHY：守卫在会话未确认（网络失败）时选择放行——授权边界在后端，不能因为一次网络
 * 抖动就把用户挡在所有页面之外。但放行之后不能立刻渲染出空数据的已登录界面，
 * 否则用户会以为"数据全空了"。登录页本身就是给未登录用户看的，不需要这道门。
 */
const showGate = computed(() => route.meta.requiresAuth !== false && session.status === 'unknown')

async function onLogout(): Promise<void> {
  if (logoutPending.value) return
  logoutPending.value = true
  try {
    // 本地状态一定会被清空（见 store 的说明）；这里只负责拿结果做提示与跳转。
    const result = await session.logout()
    if (!result.ok) {
      session.setNotice('已退出当前浏览器，但服务器未确认，请重新登录以关闭会话。')
    }
    await router.push({ name: 'teacher-login' })
  } finally {
    logoutPending.value = false
  }
}
</script>

<template>
  <AppShell
    subtitle="教师端"
    :user-name="session.displayName"
    :logout-pending="logoutPending"
    @logout="onLogout"
  >
    <ProtectedRouteGate
      v-if="showGate"
      label="正在确认登录状态…"
      hint="如果长时间没有反应，请检查网络连接。"
    />
    <RouterView v-else />
  </AppShell>
</template>
