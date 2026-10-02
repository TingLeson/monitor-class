<script setup lang="ts">
import { getCurrentInstance } from 'vue'

/**
 * 多行文本字段（课堂说明、批量粘贴学生账号）。
 *
 * WHY 与 AppTextField 并列而不是加一个 multiline 开关：两者的 DOM、可用性细节
 * （行数、resize、maxlength）都不同，用一个组件靠 if 分支拼出来只会让 props 变成
 * 一锅可选参数。共享的是"label/错误/提示"这套语义，而不是实现。
 *
 * 与 AppTextField 一致：值由父组件持有（受控），组件自己不存第二份真相。
 */
withDefaults(
  defineProps<{
    label: string
    /** 受控值：由父组件持有。 */
    modelValue: string
    placeholder?: string
    /** 可见行数；只影响初始高度，用户仍可拖动调整（resize-y）。 */
    rows?: number
    required?: boolean
    disabled?: boolean
    /** 字段级错误文案（本地校验失败）。后端错误由页面顶部或字段旁的 alert 呈现。 */
    error?: string
    /** 正常态提示（例如实时字数）。error 存在时让位给错误文案。 */
    hint?: string
    /**
     * 原生 maxlength。
     *
     * WHY 仍然传它：浏览器层面的截断能挡住"粘贴一篇文章"这最常见的一种超长输入，
     * 但它只是 UX——真正的长度校验（去空白后 1–80 等）在后端（§63）。
     */
    maxlength?: number
  }>(),
  {
    placeholder: undefined,
    rows: 4,
    required: false,
    disabled: false,
    error: undefined,
    hint: undefined,
    maxlength: undefined,
  },
)

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

/** id 取组件实例 uid：同一页面出现两个多行字段时 label 不会指错（同 AppTextField）。 */
const instanceId = getCurrentInstance()?.uid ?? 0
const textareaId = `textarea-${instanceId}`
const describedById = `${textareaId}-description`

function onInput(event: Event): void {
  emit('update:modelValue', (event.target as HTMLTextAreaElement).value)
}
</script>

<template>
  <div class="space-y-1.5">
    <label :for="textareaId" class="block text-sm font-medium text-ink">{{ label }}</label>
    <textarea
      :id="textareaId"
      :value="modelValue"
      :rows="rows"
      :placeholder="placeholder"
      :required="required"
      :disabled="disabled"
      :maxlength="maxlength"
      :aria-invalid="error ? 'true' : undefined"
      :aria-describedby="error || hint ? describedById : undefined"
      class="block w-full resize-y rounded-control border bg-surface px-3 py-2 text-sm leading-relaxed text-ink transition-colors placeholder:text-ink-muted/60 focus:outline-2 focus:outline-offset-1 focus:outline-status-open disabled:cursor-not-allowed disabled:bg-surface-muted"
      :class="error ? 'border-status-danger' : 'border-border-subtle'"
      @input="onInput"
    />
    <p v-if="error" :id="describedById" class="text-xs leading-relaxed text-status-danger">
      {{ error }}
    </p>
    <p v-else-if="hint" :id="describedById" class="text-xs leading-relaxed text-ink-muted">
      {{ hint }}
    </p>
  </div>
</template>
