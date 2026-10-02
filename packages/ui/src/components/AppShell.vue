<script setup lang="ts">
import AppButton from './AppButton.vue'

/**
 * 页面外壳（Dashboard 风格）：顶部品牌条 + 版心内容区。
 *
 * 三个 app 通过 subtitle 区分入口（学生端 / 教师端 / 管理端）。
 * WHY 外壳里没有任何"按角色显示导航"的逻辑：三个入口是物理分离的 SPA（§5），
 * 这里永远不会出现 `if role === 'TEACHER'` 这类分支。
 *
 * Phase 1 新增"当前用户 + 退出登录"区：仍然只有 props 与 emit，不认识 pinia、
 * 路由与后端——退出登录到底做什么（调接口、清 store、跳登录页）由各 app 的
 * App.vue 决定。组件一旦自己去调 store，就会变成"四个 app 共用一份业务逻辑"，
 * 与 §5 的入口隔离背道而驰。
 */
withDefaults(
  defineProps<{
    brand?: string
    subtitle?: string
    /** 当前登录用户的展示名；null / undefined 时不渲染账号区（例如登录页）。 */
    userName?: string | null
    /** 退出请求进行中：禁用按钮，避免重复提交撤销请求。 */
    logoutPending?: boolean
    /** 预留：账号区可以展示角色标签，但组件不据此改变任何行为。 */
    logoutLabel?: string
  }>(),
  {
    brand: 'ClassWatch',
    subtitle: undefined,
    userName: null,
    logoutPending: false,
    logoutLabel: '退出登录',
  },
)

const emit = defineEmits<{
  logout: []
}>()

defineSlots<{
  default?: () => unknown
  /** 右侧操作区：与账号区并排的自定义操作（后续 Phase 的导航按钮放这里）。 */
  actions?: () => unknown
}>()
</script>

<template>
  <div class="flex min-h-screen flex-col bg-surface-muted text-ink">
    <header class="border-b border-border-subtle bg-surface">
      <div class="mx-auto flex w-full max-w-5xl items-center gap-3 px-page py-4">
        <span class="h-2.5 w-2.5 rounded-full bg-status-open" aria-hidden="true" />
        <span class="text-base font-semibold tracking-tight">{{ brand }}</span>
        <span v-if="subtitle" class="text-sm text-ink-muted">{{ subtitle }}</span>
        <div class="ml-auto flex items-center gap-2">
          <slot name="actions" />
          <div
            v-if="userName"
            class="flex items-center gap-3 border-l border-border-subtle pl-3"
            data-testid="shell-account"
          >
            <span class="text-sm text-ink-muted">
              当前用户
              <span class="ml-1 font-medium text-ink" data-testid="shell-user-name">
                {{ userName }}
              </span>
            </span>
            <AppButton
              variant="secondary"
              size="sm"
              :loading="logoutPending"
              data-testid="shell-logout"
              @click="emit('logout')"
            >
              {{ logoutLabel }}
            </AppButton>
          </div>
        </div>
      </div>
    </header>

    <main class="mx-auto w-full max-w-5xl flex-1 px-page py-section">
      <slot />
    </main>
  </div>
</template>
