<script setup lang="ts">
/**
 * 空状态 / 错误状态占位。
 *
 * WHY 空状态必须有独立组件：列表为空是最容易被写成"什么都不渲染"的一种状态，
 * 用户看到一片空白时无法区分"没有数据""还在加载""请求失败了"。三种状态必须长得
 * 明显不同，而空与失败共用同一个骨架、只用语气区分。
 */
withDefaults(
  defineProps<{
    title: string
    description?: string
    /** danger 用于"加载失败"这类需要用户处理的终态。 */
    tone?: 'neutral' | 'danger'
  }>(),
  { description: undefined, tone: 'neutral' },
)

defineSlots<{
  /** 操作区（重试、清除筛选、去创建）。 */
  default?: () => unknown
}>()
</script>

<template>
  <div
    :role="tone === 'danger' ? 'alert' : 'status'"
    class="flex flex-col items-center gap-2 px-4 py-14 text-center"
  >
    <span
      class="h-1.5 w-1.5 rounded-full"
      :class="tone === 'danger' ? 'bg-status-danger' : 'bg-status-neutral'"
      aria-hidden="true"
    />
    <p class="text-sm font-medium" :class="tone === 'danger' ? 'text-status-danger' : 'text-ink'">
      {{ title }}
    </p>
    <p v-if="description" class="max-w-md text-sm leading-relaxed text-ink-muted">
      {{ description }}
    </p>
    <div v-if="$slots.default" class="mt-3">
      <slot />
    </div>
  </div>
</template>
