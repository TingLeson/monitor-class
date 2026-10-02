<script setup lang="ts">
import StatusDot from './StatusDot.vue'

/**
 * 受保护页面的加载门（Phase 1）。
 *
 * WHY 需要它：路由守卫会先 await /auth/me 再放行，但"会话未确认"还有一种情况——
 * 启动时网络失败（status 保持 unknown）。此时后端才是授权边界，守卫选择放行而不是
 * 把用户挡在门外；页面因此需要自己表达"还没确认你的身份"，绝不能先渲染出一个
 * 所有数据都是空/错误的已登录界面，让用户以为课堂列表空了。
 *
 * 组件本身不做判断：是否显示由调用方（各 app 的 App.vue / 视图）决定，
 * 它只负责"长什么样"。
 */
withDefaults(
  defineProps<{
    /** 加载中的说明文案；默认面向"正在确认登录状态"。 */
    label?: string
    /** 出错/超时后的提示，用于说明"为什么一直转"。 */
    hint?: string
  }>(),
  { label: '正在确认登录状态…', hint: undefined },
)
</script>

<template>
  <div class="mx-auto flex max-w-3xl flex-col items-center gap-3 py-16 text-center">
    <span
      class="h-5 w-5 animate-spin rounded-full border-2 border-status-closed border-t-transparent"
      aria-hidden="true"
    />
    <p class="text-sm text-ink-muted" role="status">{{ label }}</p>
    <p v-if="hint" class="flex items-center gap-2 text-xs text-ink-muted">
      <StatusDot status="warning" />
      <span>{{ hint }}</span>
    </p>
  </div>
</template>
