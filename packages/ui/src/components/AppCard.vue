<script setup lang="ts">
/**
 * 卡片容器：Dashboard 里最常见的信息单元。
 *
 * 无业务语义（不认识 Classroom / Session），只负责层次：白底 + 细边框 + 极浅阴影。
 * padded=false 留给需要自己控制内边距的内容（例如未来的视频网格）。
 */
withDefaults(
  defineProps<{
    title?: string
    description?: string
    padded?: boolean
  }>(),
  { title: undefined, description: undefined, padded: true },
)

defineSlots<{
  default?: () => unknown
  header?: () => unknown
  footer?: () => unknown
}>()
</script>

<template>
  <section
    class="rounded-card border border-border-subtle bg-surface shadow-card"
    :class="padded ? 'p-6' : ''"
  >
    <header
      v-if="title || description || $slots.header"
      class="mb-4 flex items-start justify-between gap-4"
    >
      <div class="space-y-1">
        <slot name="header">
          <h2 v-if="title" class="text-lg font-semibold tracking-tight">{{ title }}</h2>
          <p v-if="description" class="max-w-2xl text-sm leading-relaxed text-ink-muted">
            {{ description }}
          </p>
        </slot>
      </div>
    </header>

    <slot />

    <footer v-if="$slots.footer" class="mt-6 border-t border-border-subtle pt-4">
      <slot name="footer" />
    </footer>
  </section>
</template>
