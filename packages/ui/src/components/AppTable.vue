<script setup lang="ts">
/**
 * 语义化表格外壳。
 *
 * WHY 仍然用 `<table>` 而不是 div 网格：账号列表是行列对齐的表格数据，`<table>` +
 * `<caption>` + `<th>` 是读屏软件唯一能正确播报"第几行第几列、列名是什么"的结构。
 * §35 禁止的是**满屏表格线**那种老式后台观感，不是表格语义本身。
 *
 * 视觉上因此只保留两条线：表头下一条、每行之间一条；列间距靠 padding，
 * 不用竖线网格。单元格样式由根节点的任意变体（`[&_td]` / `[&_th]`）统一下发，
 * 页面写裸 `<td>` 即可——避免每个页面重复一遍 padding，也就不会各自漂移。
 */
withDefaults(
  defineProps<{
    /** 表格标题（读屏用，视觉上隐藏）。必填：没有标题的表格对读屏用户是一组无意义的数字。 */
    label: string
  }>(),
  {},
)

defineSlots<{
  /** 表头行的 `<th>` 列表。 */
  head?: () => unknown
  /** 表体行的 `<tr>` 列表。 */
  default?: () => unknown
  /** 表尾（合计等）。 */
  foot?: () => unknown
}>()
</script>

<template>
  <div class="overflow-x-auto">
    <table
      class="w-full border-collapse text-left text-sm [&_tbody_tr]:border-t [&_tbody_tr]:border-border-subtle [&_td]:px-3 [&_td]:py-3 [&_td]:align-middle [&_th]:px-3 [&_th]:py-2.5 [&_th]:font-medium [&_tfoot_tr]:border-t [&_tfoot_tr]:border-border-subtle"
    >
      <caption class="sr-only">
        {{
          label
        }}
      </caption>
      <thead class="text-xs tracking-wide text-ink-muted uppercase">
        <tr>
          <slot name="head" />
        </tr>
      </thead>
      <tbody>
        <slot />
      </tbody>
      <tfoot v-if="$slots.foot">
        <slot name="foot" />
      </tfoot>
    </table>
  </div>
</template>
