<script setup lang="ts">
import { computed } from 'vue'
import AppCard from './AppCard.vue'
import StatusDot from './StatusDot.vue'

/**
 * Phase 0 页面占位。
 *
 * WHY 不做成空白页 / `// TODO: implement`：每个路由都必须交代清"这一页负责什么、
 * 哪个 Phase 实现、未来必须守住哪些约束（权限、状态机、隐私）"。这份说明是给下一个
 * Phase 的执行者（人或 Agent）的接口契约，也让人工验收能一眼看出骨架是否与 §55 对齐。
 */
const props = withDefaults(
  defineProps<{
    title: string
    /** 一句话职责，取自任务书对应章节。 */
    description: string
    /** 计划实现该页面的 Phase，例如 'Phase 1'。 */
    phase: string
    /** 未来必须守住的约束（权限、状态机、隐私要求等），逐条列出。 */
    notes?: string[]
    /** 当前路由 path：用于自检"页面显示的路径与实际 URL 一致"。 */
    path?: string
  }>(),
  { notes: undefined, path: undefined },
)

const hasNotes = computed(() => (props.notes?.length ?? 0) > 0)
</script>

<template>
  <AppCard>
    <div class="space-y-2">
      <p v-if="path" class="font-mono text-xs text-ink-muted">{{ path }}</p>
      <h1 class="text-2xl font-semibold tracking-tight">{{ title }}</h1>
      <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">{{ description }}</p>
    </div>

    <p
      class="mt-5 inline-flex items-center gap-2 rounded-control border border-border-subtle bg-surface-muted px-3 py-1.5 text-xs text-ink-muted"
    >
      <StatusDot status="neutral" />
      <span>将在 {{ phase }} 实现</span>
    </p>

    <ul v-if="hasNotes" class="mt-6 space-y-2 border-t border-border-subtle pt-5">
      <li v-for="note in notes" :key="note" class="flex gap-2 text-sm">
        <span class="mt-2 h-1 w-1 shrink-0 rounded-full bg-status-closed" aria-hidden="true" />
        <span class="leading-relaxed text-ink-muted">{{ note }}</span>
      </li>
    </ul>
  </AppCard>
</template>
