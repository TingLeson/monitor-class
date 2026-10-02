<script lang="ts">
/**
 * 提示条语义（§35 低噪声）：
 * - danger  → 操作失败、加载失败（必须让用户看到）
 * - success → 操作成功的一次性确认（例如"账号已创建"）
 * - info    → 说明性文字（例如"为什么没有管理员选项"）
 */
export type AlertTone = 'danger' | 'success' | 'info'
</script>

<script setup lang="ts">
/**
 * 行内提示条。三个管理端页面都要"把结果或原因说清楚"，把它抽成组件是为了让
 * 危险/成功/说明三种语气在三处保持一致，而不是每页各写一份颜色。
 *
 * role 的选择决定读屏行为：danger 用 alert（立即打断朗读），其余用 status（排队朗读）。
 * 一次网络失败不该被读成"请稍候"，一次成功提示也不该打断用户正在读的内容。
 */
withDefaults(
  defineProps<{
    tone?: AlertTone
    /** 可选的粗体标题；正文放默认插槽。 */
    title?: string
  }>(),
  { tone: 'info', title: undefined },
)

const TONE_CLASSES: Record<AlertTone, string> = {
  danger: 'border-status-danger/30 bg-status-danger/5 text-status-danger',
  success: 'border-status-open/30 bg-status-open/5 text-status-open',
  info: 'border-border-subtle bg-surface text-ink-muted',
}
</script>

<template>
  <div
    :role="tone === 'danger' ? 'alert' : 'status'"
    class="flex items-start gap-3 rounded-control border px-3 py-2 text-sm leading-relaxed"
    :class="TONE_CLASSES[tone]"
  >
    <div class="flex-1 space-y-0.5">
      <p v-if="title" class="font-medium">{{ title }}</p>
      <div><slot /></div>
    </div>
    <div v-if="$slots.actions" class="flex shrink-0 items-center gap-2">
      <slot name="actions" />
    </div>
  </div>
</template>
