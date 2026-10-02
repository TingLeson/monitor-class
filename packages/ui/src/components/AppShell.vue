<script setup lang="ts">
/**
 * 页面外壳（Dashboard 风格）：顶部品牌条 + 版心内容区。
 *
 * 三个 app 通过 subtitle 区分入口（学生端 / 教师端 / 管理端）。
 * WHY 外壳里没有任何"按角色显示导航"的逻辑：三个入口是物理分离的 SPA（§5），
 * 这里永远不会出现 `if role === 'TEACHER'` 这类分支。
 */
withDefaults(
  defineProps<{
    brand?: string
    subtitle?: string
  }>(),
  { brand: 'ClassWatch', subtitle: undefined },
)

defineSlots<{
  default?: () => unknown
  /** 右侧操作区（Phase 1 起放账号菜单 / 退出登录）。 */
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
        </div>
      </div>
    </header>

    <main class="mx-auto w-full max-w-5xl flex-1 px-page py-section">
      <slot />
    </main>
  </div>
</template>
