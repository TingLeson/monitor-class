<script lang="ts">
/**
 * 下拉选项。
 *
 * WHY value 一律是字符串、组件不认识 Role / UserStatus：共享组件一旦 import 业务
 * 枚举，三个入口就被同一个组件绑在一起（§5）。调用方负责把领域值映射成字符串，
 * 读回来时用 isRole / isUserStatus 这类守卫校验——URL 与用户输入都是不可信来源，
 * 校验必须发生在应用层。
 */
export interface SelectOption {
  value: string
  label: string
}
</script>

<script setup lang="ts">
import { getCurrentInstance } from 'vue'

withDefaults(
  defineProps<{
    label: string
    modelValue: string
    options: SelectOption[]
    required?: boolean
    disabled?: boolean
    /** 字段级错误文案（本地校验失败）。 */
    error?: string
    hint?: string
  }>(),
  { required: false, disabled: false, error: undefined, hint: undefined },
)

const emit = defineEmits<{
  'update:modelValue': [value: string]
}>()

/** 同 AppTextField：用实例 uid 生成 id，避免同页面多个下拉的 label 指错。 */
const instanceId = getCurrentInstance()?.uid ?? 0
const selectId = `select-${instanceId}`
const describedById = `${selectId}-description`

function onChange(event: Event): void {
  emit('update:modelValue', (event.target as HTMLSelectElement).value)
}
</script>

<template>
  <div class="space-y-1.5">
    <label :for="selectId" class="block text-sm font-medium text-ink">{{ label }}</label>
    <select
      :id="selectId"
      :value="modelValue"
      :required="required"
      :disabled="disabled"
      :aria-invalid="error ? 'true' : undefined"
      :aria-describedby="error || hint ? describedById : undefined"
      class="h-10 w-full rounded-control border bg-surface px-2.5 text-sm text-ink transition-colors focus:outline-2 focus:outline-offset-1 focus:outline-status-open disabled:cursor-not-allowed disabled:bg-surface-muted"
      :class="error ? 'border-status-danger' : 'border-border-subtle'"
      @change="onChange"
    >
      <option v-for="option in options" :key="option.value" :value="option.value">
        {{ option.label }}
      </option>
    </select>
    <p v-if="error" :id="describedById" class="text-xs leading-relaxed text-status-danger">
      {{ error }}
    </p>
    <p v-else-if="hint" :id="describedById" class="text-xs leading-relaxed text-ink-muted">
      {{ hint }}
    </p>
  </div>
</template>
