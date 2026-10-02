<script lang="ts">
/**
 * 状态色语义（§35 清晰状态颜色），与任务书状态枚举对齐：
 * - open    → Classroom OPEN / StudentSession ONLINE
 * - closed  → Classroom CLOSED、学生已离开等终态
 * - danger  → SCREEN_LOST / DISCONNECTED（老师必须立刻看到）
 * - warning → 需要关注但不致命（网络较差、摄像头异常）
 * - neutral → 未知 / 尚未收到事件（不要用绿色兜底，否则老师会看到假"正常"）
 */
export type StatusTone = 'open' | 'closed' | 'danger' | 'warning' | 'neutral'
</script>

<script setup lang="ts">
withDefaults(
  defineProps<{
    status: StatusTone
    label?: string
    /** 呼吸动画：只用于"正在进行中"的状态，静态状态不要动。 */
    pulse?: boolean
  }>(),
  { label: undefined, pulse: false },
)

const TONE_CLASSES: Record<StatusTone, string> = {
  open: 'bg-status-open',
  closed: 'bg-status-closed',
  danger: 'bg-status-danger',
  warning: 'bg-status-warning',
  neutral: 'bg-status-neutral',
}
</script>

<template>
  <span class="inline-flex items-center gap-2 text-sm">
    <span
      class="h-2 w-2 shrink-0 rounded-full"
      :class="[TONE_CLASSES[status], pulse ? 'animate-pulse' : '']"
      aria-hidden="true"
    />
    <span v-if="label" class="text-ink-muted">{{ label }}</span>
  </span>
</template>
