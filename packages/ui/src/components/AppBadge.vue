<script lang="ts">
/**
 * 徽章语义色（§35 清晰状态色）。
 *
 * WHY 与 StatusDot 的 StatusTone 分开：StatusDot 表达的是"运行状态"（open/closed/
 * danger/warning/neutral），徽章表达的是"一个分类标签"（角色、账号状态），
 * 两者语义不同。共用一套 tone 会逼着调用方把"停用的账号"映射成 closed，
 * 或者把"管理员"映射成一个状态色——那正是 §35 要避免的模板感。
 */
export type BadgeTone = 'neutral' | 'positive' | 'muted' | 'danger' | 'attention' | 'strong'
</script>

<script setup lang="ts">
/**
 * 低噪声徽章：一个圆点 + 一行字，用于角色（ADMIN/TEACHER/STUDENT）与账号状态
 * （ACTIVE/DISABLED）。组件不认识 Role / UserStatus，映射由页面负责——
 * 共享组件一旦认识业务枚举，就会变成"四个 app 共用一份业务规则"（§5）。
 */
withDefaults(
  defineProps<{
    label: string
    tone?: BadgeTone
  }>(),
  { tone: 'neutral' },
)

const TONE_CLASSES: Record<BadgeTone, string> = {
  neutral: 'border-border-subtle bg-surface text-ink-muted',
  positive: 'border-status-open/25 bg-status-open/10 text-status-open',
  muted: 'border-border-subtle bg-surface-muted text-ink-muted',
  danger: 'border-status-danger/25 bg-status-danger/10 text-status-danger',
  attention: 'border-status-warning/25 bg-status-warning/10 text-status-warning',
  // 深一档的中性色：用于"角色"这类需要区分层级、但**不能**借用状态色的标签
  // （把"管理员"涂成绿色，用户会读成"在线/正常"）。
  strong: 'border-ink/15 bg-ink/5 text-ink',
}

const DOT_CLASSES: Record<BadgeTone, string> = {
  neutral: 'bg-status-neutral',
  positive: 'bg-status-open',
  muted: 'bg-status-closed',
  danger: 'bg-status-danger',
  attention: 'bg-status-warning',
  strong: 'bg-ink',
}
</script>

<template>
  <span
    class="inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap"
    :class="TONE_CLASSES[tone]"
  >
    <span class="h-1.5 w-1.5 shrink-0 rounded-full" :class="DOT_CLASSES[tone]" aria-hidden="true" />
    {{ label }}
  </span>
</template>
