<script setup lang="ts">
import type { MonitorStudent } from '@classwatch/shared-types'
import { AppBadge } from '@classwatch/ui'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import type { ScreenSubscription } from '../lib/media/media-room.ts'
import {
  TILE_BODY_TEXT,
  describeMonitorBadge,
  describeTileBody,
  type MonitorMediaState,
} from '../lib/monitor-status.ts'

/**
 * 监督卡片（§29 / §51 / §52）—— Phase 6 的简单形态。
 *
 * 卡片由两部分拼成，界线必须清楚：
 * - **业务状态**（徽章、是否该有画面）来自 `student`（Monitor DTO，§51）；
 * - **媒体状态**（画面到没到）来自 `subscription` / `mediaState`（LiveKit）。
 *
 * 桌面屏幕是卡片主体（§29），摄像头画中画属于 Phase 9、Focus View 属于 Phase 7，
 * 这里都不做（§54：不提前实现）。
 *
 * `<video>` 的三个属性各有原因：
 * - `autoplay`：订阅一到位就要出画面，老师不该再点一次播放；
 * - `playsinline`：避免 iOS/Safari 抢成全屏播放器；
 * - `muted`：**显式**静音。屏幕轨道通常没有音频，但一旦哪天带上了，
 *   老师端把学生桌面的声音外放出来就是一个回声源（老师说话 → 学生麦克风 → 又回到
 *   老师这边）。所以这里宁可显式写死 `muted`，也不留"看起来没事"的默认值。
 */
const props = defineProps<{
  student: MonitorStudent
  /** 已建立的订阅；null 表示这个学生当前没有可播放的画面。 */
  subscription: ScreenSubscription | null
  mediaState: MonitorMediaState
}>()

const videoRef = ref<HTMLVideoElement | null>(null)

/**
 * 当前的 detach 函数。
 *
 * 放在闭包而不是 state：它是一个"清理动作"，不是界面要渲染的数据；
 * 放进响应式 state 还会让 Vue 去代理一个持有媒体对象的闭包。
 */
let detach: (() => void) | null = null

function detachCurrent(): void {
  detach?.()
  detach = null
}

/**
 * 订阅变化或 `<video>` 挂载/卸载时重新 attach。
 *
 * `flush: 'post'` 是必须的：`<video>` 是 `v-if` 出来的，只有在 DOM 更新之后
 * `videoRef` 才有值；用默认的 pre 时机会出现"第一次总是挂不上，要等下一次刷新"
 * 这种只在真机上才看得见的间歇故障。
 */
watch(
  [() => props.subscription, videoRef],
  ([subscription]) => {
    detachCurrent()
    if (subscription === null || videoRef.value === null) return
    detach = subscription.attach(videoRef.value)
  },
  { immediate: true, flush: 'post' },
)

onBeforeUnmount(detachCurrent)

/** §29 的三种徽章：🟢 正常 / 🔴 屏幕中断 / ⚪ 未连接。 */
const badge = computed(() => describeMonitorBadge(props.student))

/** 卡片主体该显示画面还是占位（四种"没画面"的原因必须区分，见 monitor-status.ts）。 */
const body = computed(() =>
  describeTileBody(props.student, {
    state: props.mediaState,
    hasSubscription: props.subscription !== null,
  }),
)

const placeholderText = computed(() => (body.value === 'live' ? null : TILE_BODY_TEXT[body.value]))
</script>

<template>
  <article
    class="overflow-hidden rounded-card border border-border-subtle bg-surface shadow-card"
    data-testid="monitor-tile"
    :data-session-id="student.sessionId ?? ''"
  >
    <header class="flex items-center justify-between gap-2 px-4 py-3">
      <h2 class="truncate text-sm font-medium text-ink" data-testid="tile-name">
        {{ student.displayName }}
      </h2>
      <AppBadge
        :label="`${badge.emoji} ${badge.label}`"
        :tone="badge.tone"
        data-testid="tile-badge"
      />
    </header>

    <!-- 桌面屏幕是卡片主体（§29）；没有画面时给出**具体原因**，而不是一个黑框。 -->
    <div class="aspect-video bg-surface-muted">
      <video
        v-if="body === 'live'"
        ref="videoRef"
        class="h-full w-full object-contain"
        autoplay
        playsinline
        muted
        data-testid="tile-video"
      />
      <div v-else class="flex h-full items-center justify-center px-4 text-center">
        <p class="text-sm text-ink-muted" data-testid="tile-placeholder">{{ placeholderText }}</p>
      </div>
    </div>

    <footer class="flex items-center justify-between gap-2 border-t border-border-subtle px-4 py-2">
      <span class="text-xs text-ink-muted" data-testid="tile-connection">
        连接：{{ student.connection === 'GOOD' ? '正常' : '未知' }}
      </span>
      <span v-if="student.camera.active" class="text-xs text-ink-muted">摄像头已开启</span>
    </footer>
  </article>
</template>
