<script setup lang="ts">
import { getCurrentInstance, nextTick, onBeforeUnmount, ref, watch } from 'vue'

/**
 * 弹窗（确认 / 表单 / 一次性密码展示）。
 *
 * WHY 自己写而不是引 UI 框架：项目禁止引入 UI 框架（§35 只允许 Tailwind 或轻量
 * headless 组件），而这里需要的只是一个对话框骨架；引入一个组件库为了一个弹窗，
 * 会把整套设计语言带进来，和"低噪声"的目标相反。
 *
 * WHY 用 `v-if` 而不是 `v-show`：`v-show` 会把内容留在 DOM 里。一次性密码、
 * 隐藏的账号信息都属于"关闭后必须真的从 DOM 消失"的内容，`v-if` 让这件事由结构
 * 保证，而不是靠调用方记得清空。
 *
 * WHY 不 Teleport：本组件的每个使用点都挂在视图根部（没有 transform / overflow
 * 祖先），`fixed` 遮罩已经足够；不 Teleport 让"关闭后 DOM 里是否还有那段密码"
 * 可以直接在组件树上断言，少一层全局 DOM 副作用。将来若有弹窗必须从滚动容器内部
 * 打开，再评估 Teleport。
 */
const props = withDefaults(
  defineProps<{
    open: boolean
    title: string
    description?: string
    /** danger 用于不可逆操作（停用账号、重置密码），让标题与主按钮的语气一致。 */
    tone?: 'default' | 'danger'
    /**
     * 是否允许 ESC 与点击遮罩关闭。
     * 危险确认设为 false：一次误点遮罩就把操作丢掉，用户会以为是"点了没反应"。
     */
    dismissible?: boolean
  }>(),
  { description: undefined, tone: 'default', dismissible: true },
)

const emit = defineEmits<{ close: [] }>()

const panel = ref<HTMLElement | null>(null)

/**
 * aria-labelledby / aria-describedby 指向的 id 用实例 uid 生成（同 AppTextField）：
 * 同一页面可能同时存在多个弹窗组件实例，固定 id 会让读屏软件读错标题。
 */
const instanceId = getCurrentInstance()?.uid ?? 0
const titleId = `modal-title-${instanceId}`
const descriptionId = `modal-description-${instanceId}`

function requestClose(): void {
  if (!props.dismissible) return
  emit('close')
}

/**
 * ESC 监听挂在 document 而不是面板上。
 *
 * 面板监听只在焦点位于弹窗内部时有效；用户点过遮罩后焦点会落到 body，
 * 此时 ESC 就静默失效了——键盘用户会以为弹窗卡住。
 */
function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') requestClose()
}

watch(
  () => props.open,
  async (open) => {
    if (open) {
      document.addEventListener('keydown', onKeydown)
      // 面板获得焦点：读屏软件会朗读 aria-labelledby 指定的标题，
      // 键盘用户的第一次 Tab 也从弹窗内部开始，不会跑到背后的页面上。
      await nextTick()
      panel.value?.focus()
      return
    }
    document.removeEventListener('keydown', onKeydown)
  },
  { immediate: true },
)

onBeforeUnmount(() => document.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div
    v-if="open"
    class="fixed inset-0 z-50 flex items-center justify-center overflow-y-auto bg-ink/40 p-4"
    data-testid="modal-overlay"
    @click.self="requestClose"
  >
    <div
      ref="panel"
      role="dialog"
      aria-modal="true"
      tabindex="-1"
      :aria-labelledby="titleId"
      :aria-describedby="description ? descriptionId : undefined"
      class="w-full max-w-md rounded-card border border-border-subtle bg-surface p-6 shadow-card outline-none"
    >
      <h2
        :id="titleId"
        class="text-lg font-semibold tracking-tight"
        :class="tone === 'danger' ? 'text-status-danger' : 'text-ink'"
      >
        {{ title }}
      </h2>
      <p v-if="description" :id="descriptionId" class="mt-2 text-sm leading-relaxed text-ink-muted">
        {{ description }}
      </p>

      <div class="mt-5">
        <slot />
      </div>

      <div v-if="$slots.footer" class="mt-6 flex justify-end gap-2">
        <slot name="footer" />
      </div>
    </div>
  </div>
</template>
