<script setup lang="ts">
import { computed } from 'vue'
import AppButton from './AppButton.vue'

/**
 * 分页控件（上一页 / 下一页 + 总数）。
 *
 * WHY 只做上一页/下一页，不做页码列表：V1 的账号列表按"搜索 + 筛选"使用，
 * 用户几乎不会翻到第 7 页；页码列表在没有"跳页"需求时只是一排噪声（§35）。
 *
 * 计数文案同时给出"当前区间"与"总数"：只显示"共 123 条"时，用户无法判断
 * 自己是不是已经翻到底；只显示"第 2 页"时，又不知道还剩多少。
 */
const props = withDefaults(
  defineProps<{
    page: number
    pageSize: number
    total: number
    /** 计数单位，例如"个账号"；默认"条"。 */
    unit?: string
  }>(),
  { unit: '条' },
)

const emit = defineEmits<{ 'update:page': [page: number] }>()

const pageCount = computed(() => Math.max(1, Math.ceil(props.total / Math.max(1, props.pageSize))))

/**
 * 用于显示与区间计算的页码。
 *
 * WHY 需要钳制：page 可能来自手改的地址栏（后端对越界页码不做纠正，只会返回一页空数据）。
 * 不钳制就会出现"第 4901–120 条 / 共 120 条"这种不可能的数字——用户会以为系统坏了，
 * 而真相只是"你翻过头了"。按钮点击的目标页码同样经过钳制，一点就回到有效范围。
 */
const safePage = computed(() => Math.min(Math.max(1, props.page), pageCount.value))

/** 当前页第一条的序号；总数为 0 时显示 0，而不是 1。 */
const rangeStart = computed(() =>
  props.total === 0 ? 0 : (safePage.value - 1) * props.pageSize + 1,
)
const rangeEnd = computed(() => Math.min(safePage.value * props.pageSize, props.total))

const canPrev = computed(() => safePage.value > 1)
const canNext = computed(() => safePage.value < pageCount.value)

function goTo(page: number): void {
  // 边界钳制放在这里：即使调用方传了 0、pageCount+1 或 99，也不会发出越界请求。
  const next = Math.min(Math.max(1, page), pageCount.value)
  if (next === props.page) return
  emit('update:page', next)
}
</script>

<template>
  <nav class="flex flex-wrap items-center justify-between gap-3" aria-label="分页">
    <p class="text-sm text-ink-muted" data-testid="pagination-summary">
      <template v-if="total > 0">
        第 {{ rangeStart }}–{{ rangeEnd }} {{ unit }} / 共 {{ total }} {{ unit }}
      </template>
      <template v-else>共 0 {{ unit }}</template>
    </p>

    <div class="flex items-center gap-2">
      <span class="text-sm text-ink-muted">第 {{ safePage }} / {{ pageCount }} 页</span>
      <AppButton
        variant="secondary"
        size="sm"
        :disabled="!canPrev"
        data-testid="pagination-prev"
        @click="goTo(page - 1)"
      >
        上一页
      </AppButton>
      <AppButton
        variant="secondary"
        size="sm"
        :disabled="!canNext"
        data-testid="pagination-next"
        @click="goTo(page + 1)"
      >
        下一页
      </AppButton>
    </div>
  </nav>
</template>
