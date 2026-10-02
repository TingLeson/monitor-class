<script setup lang="ts">
import { getCurrentInstance } from 'vue'

withDefaults(
  defineProps<{
    label: string
    /**
     * input 的 autocomplete 值。密码必须是 `current-password`：
     * 写错（或用 `new-password`）会让浏览器密码管理器不填充、反而弹出"保存新密码"，
     * 这是登录表单最常见也最难自查的可用性缺陷。
     */
    autocomplete?: string
    type?: 'text' | 'password'
    placeholder?: string
    required?: boolean
    disabled?: boolean
    /** 字段级错误文案（本地校验失败）。后端错误统一由页面顶部的 alert 呈现。 */
    error?: string
    hint?: string
    /** 受控值：由父组件持有，避免组件内部再存一份状态造成两份真相。 */
    modelValue: string
  }>(),
  {
    autocomplete: undefined,
    type: 'text',
    placeholder: undefined,
    required: false,
    disabled: false,
    error: undefined,
    hint: undefined,
  },
)

const emit = defineEmits<{
  'update:modelValue': [value: string]
  enter: []
}>()

/**
 * id 取 Vue 的实例 uid，而不是"模块级自增计数"。
 *
 * WHY：label 的 `for` 一旦指错，点标签会聚焦到别的输入框，是典型的可用性事故。
 * 实例 uid 在整个应用内唯一（同一页面渲染两个"账号"字段也不会撞），
 * 而且不依赖模块级可变状态——那种写法在"同一测试进程里多次 mount 同一个组件"
 * 时会重置计数、生成重复 id（组件测试已经踩到过这个坑）。
 */
const instanceId = getCurrentInstance()?.uid ?? 0
const inputId = `field-${instanceId}`
const describedById = `${inputId}-description`

function onInput(event: Event): void {
  emit('update:modelValue', (event.target as HTMLInputElement).value)
}
</script>

<template>
  <div class="space-y-1.5">
    <label :for="inputId" class="block text-sm font-medium text-ink">{{ label }}</label>
    <input
      :id="inputId"
      :type="type"
      :value="modelValue"
      :placeholder="placeholder"
      :autocomplete="autocomplete"
      :required="required"
      :disabled="disabled"
      :aria-invalid="error ? 'true' : undefined"
      :aria-describedby="error || hint ? describedById : undefined"
      class="h-10 w-full rounded-control border bg-surface px-3 text-sm text-ink transition-colors placeholder:text-ink-muted/60 focus:outline-2 focus:outline-offset-1 focus:outline-status-open disabled:cursor-not-allowed disabled:bg-surface-muted"
      :class="error ? 'border-status-danger' : 'border-border-subtle'"
      @input="onInput"
      @keyup.enter="emit('enter')"
    />
    <p v-if="error" :id="describedById" class="text-xs leading-relaxed text-status-danger">
      {{ error }}
    </p>
    <p v-else-if="hint" :id="describedById" class="text-xs leading-relaxed text-ink-muted">
      {{ hint }}
    </p>
  </div>
</template>
