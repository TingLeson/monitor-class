<script setup lang="ts">
import { AppShell, ProtectedRouteGate } from '@classwatch/ui'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { useRealtimeStore } from './stores/realtime'
import { useSessionStore } from './stores/session'

/**
 * 学生端根组件：只负责"外壳 + 路由出口 + 顶栏账号区"。
 *
 * WHY 这里没有任何角色判断：学生端是一个物理独立的 SPA（§5），
 * 它的进程里不存在教师/管理员的页面代码，因此也就不可能因为前端 bug
 * 把教师界面渲染给学生。渲染的只有后端返回的 displayName。
 */
const session = useSessionStore()
const realtime = useRealtimeStore()
const route = useRoute()
const router = useRouter()
const logoutPending = ref(false)

/**
 * 实时通道跟随登录态（§47）。
 *
 * WHY 放在根组件而不是某个页面里：
 * - 一个入口只需要**一条**连接。放在页面里，学生从列表进课堂就会关掉再开一次，
 *   每次导航都有一段"收不到事件"的空窗；
 * - 未登录时不连（那是一条注定被拒的连接），登出后必须关（那是一条已经不被授权的通道）。
 *
 * `immediate: true` 让首帧就对齐：已经在登录态时立即开始连接，而不是等状态变化。
 */
watch(
  () => session.isAuthenticated,
  (authenticated) => {
    if (authenticated) realtime.start()
    else realtime.stop()
  },
  { immediate: true },
)

/** 根组件卸载（页面关闭、HMR 重建）时不留悬挂的连接。 */
onBeforeUnmount(() => {
  realtime.stop()
})

/**
 * 只在"需要登录、但会话尚未确认"时显示加载态。
 *
 * WHY：守卫在会话未确认（网络失败）时选择放行——授权边界在后端，不能因为一次网络
 * 抖动就把用户挡在所有页面之外。但放行之后不能立刻渲染出空数据的已登录界面，
 * 否则用户会以为"我的课堂空了"。登录页本身就是给未登录用户看的，不需要这个门。
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
    await router.push({ name: 'student-login' })
  } finally {
    logoutPending.value = false
  }
}
</script>

<template>
  <AppShell
    subtitle="学生端"
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
