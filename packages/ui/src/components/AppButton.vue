<script lang="ts">
/**
 * 按钮变体（§35 低噪声原则）：一个界面里 primary 只应出现一次，
 * 危险操作（关闭课堂、删除学生授权）才用 danger，其余用 secondary / ghost。
 */
export type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost'

export type ButtonSize = 'sm' | 'md'
</script>

<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    variant?: ButtonVariant
    size?: ButtonSize
    type?: 'button' | 'submit' | 'reset'
    disabled?: boolean
    loading?: boolean
    /** 占满父容器宽度：表单提交、卡片主操作用。 */
    block?: boolean
  }>(),
  {
    variant: 'primary',
    size: 'md',
    type: 'button',
    disabled: false,
    loading: false,
    block: false,
  },
)

/**
 * loading 与 disabled 等效。
 * WHY 现在就做：登录、开课、join 都是"点击后要等后端"的操作，
 * 若不在组件层统一拦截，每个页面都要自己写一遍防重复提交。
 */
const isDisabled = computed(() => props.disabled || props.loading)

const VARIANT_CLASSES: Record<ButtonVariant, string> = {
  primary: 'bg-ink text-surface hover:bg-ink/90',
  secondary: 'border border-border-subtle bg-surface text-ink hover:bg-surface-muted',
  danger: 'bg-status-danger text-surface hover:bg-status-danger/90',
  ghost: 'text-ink-muted hover:bg-surface-muted hover:text-ink',
}

const SIZE_CLASSES: Record<ButtonSize, string> = {
  sm: 'h-8 px-3 text-xs',
  md: 'h-10 px-4 text-sm',
}
</script>

<template>
  <button
    :type="type"
    :disabled="isDisabled"
    :aria-busy="loading ? 'true' : undefined"
    class="inline-flex items-center justify-center gap-2 rounded-control font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-status-open disabled:cursor-not-allowed disabled:opacity-50"
    :class="[VARIANT_CLASSES[variant], SIZE_CLASSES[size], block ? 'w-full' : '']"
  >
    <span
      v-if="loading"
      class="h-3.5 w-3.5 animate-spin rounded-full border-2 border-current border-t-transparent"
      aria-hidden="true"
    />
    <slot />
  </button>
</template>
