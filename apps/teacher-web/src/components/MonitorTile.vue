<script setup lang="ts">
import type { MonitorStudent } from '@classwatch/shared-types'
import { AppBadge } from '@classwatch/ui'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { ScreenSubscription } from '../lib/media/media-room.ts'
import {
  TILE_BODY_HINT,
  TILE_BODY_TEXT,
  deriveMonitorTileState,
  describeConnectionHint,
  describeMonitorBadge,
  describeTileBody,
  type MonitorMediaState,
} from '../lib/monitor-status.ts'
import { createTileVisibility, type TileVisibilityHandle } from '../lib/tile-visibility.ts'

/**
 * 监督卡片（§29 / §51 / §52 / §73）。
 *
 * 卡片由两部分拼成，界线必须清楚：
 * - **业务状态**（徽章、是否该有画面）来自 `student`（Monitor DTO，§51）；
 * - **媒体状态**（画面到没到）来自 `subscription` / `mediaState`（LiveKit）。
 *
 * 卡片同时是 §52 动态订阅的**输入端**：它把自己的可见性报给父组件（再进 store），
 * "哪些卡片在视口里"这件事只有 DOM 知道，而"要不要订"是媒体策略，属于 store。
 * 这里刻意不做任何订阅判断——组件只回答事实。
 *
 * `<video>` 的三个属性各有原因：
 * - `autoplay`：订阅一到位就要出画面，老师不该再点一次播放；
 * - `playsinline`：避免 iOS/Safari 抢成全屏播放器；
 * - `muted`：**显式**静音。屏幕轨道通常没有音频，但一旦哪天带上了，
 *   老师端把学生桌面的声音外放出来就是一个回声源（老师说话 → 学生麦克风 → 又回到
 *   老师这边）。所以这里宁可显式写死 `muted`，也不留"看起来没事"的默认值。
 *
 * 摄像头画中画（§29 的右下角 CAM 小窗）属于 Phase 9，这里**不**预留空框：
 * 一个永远空着的框会被老师读成"摄像头坏了"。页脚那行"摄像头已开启"用的是
 * DTO 里真实存在的 `camera.active`，不是假 UI。
 */
const props = defineProps<{
  student: MonitorStudent
  /** 已建立的订阅；null 表示这个学生当前没有可播放的画面。 */
  subscription: ScreenSubscription | null
  mediaState: MonitorMediaState
  /** 这张卡片是不是当前 Focus 的对象（只影响 aria 与高亮，不影响订阅）。 */
  selected?: boolean
}>()

const emit = defineEmits<{
  /** 老师要放大这个学生（点击或键盘激活）。带上事件是为了让父组件能把焦点还回来。 */
  open: [event: Event]
  /** 这张卡片进入了/离开了视口（§52 动态订阅的输入）。 */
  visibilityChange: [visible: boolean]
}>()

const rootRef = ref<HTMLElement | null>(null)
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

/* -------------------------------------------------------------------------- */
/* 可见性（§52）                                                              */
/* -------------------------------------------------------------------------- */

let visibility: TileVisibilityHandle | null = null
/**
 * 上一次报出去的可见性。
 *
 * 为什么卸载时要主动报一次 `false`：卡片从名单里消失时不会再有 IntersectionObserver
 * 回调，父组件那份"在视口里的学生"集合就会留着这个 id。订阅计划虽然会按名单过滤掉它，
 * 但让集合随着卡片生命周期收敛，才不会在老师反复增删学生后慢慢变成一份历史记录。
 */
let lastVisible = false

onMounted(() => {
  const element = rootRef.value
  if (element === null) return
  visibility = createTileVisibility(props.student.studentId, (visible) => {
    lastVisible = visible
    emit('visibilityChange', visible)
  })
  visibility.observe(element)
})

onBeforeUnmount(() => {
  visibility?.disconnect()
  visibility = null
  if (lastVisible) emit('visibilityChange', false)
})

/** 键盘激活（Enter / Space）：卡片是 role="button"，必须两种键都认。 */
function activate(event: KeyboardEvent): void {
  if (event.key !== 'Enter' && event.key !== ' ') return
  // Space 默认会滚动页面；卡片被当成按钮用，就不能让页面跳一下。
  event.preventDefault()
  emit('open', event)
}

/** §29 的徽章：🟢 正常 / 🔴 屏幕中断 / 🟡 连接中 / ⚪ 已断开 / ⚫ 已离开 / ⚪ 未进入。 */
const badge = computed(() => describeMonitorBadge(props.student))

/**
 * 派生展示状态（§29）。
 *
 * 除徽章外还写进 `data-tile-state`：监督墙出问题时，老师截一张图就能说清
 * "这一格当时是哪一档"，而不必复述徽章上的中文。`data-session-id` 同理——
 * 它把卡片与 LiveKit identity 对上，真机排障时可以在 DevTools 里直接比对。
 */
const tileState = computed(() => deriveMonitorTileState(props.student))

/** 卡片主体该显示画面还是占位（七种"没画面"的原因必须区分，见 monitor-status.ts）。 */
const body = computed(() =>
  describeTileBody(props.student, {
    state: props.mediaState,
    hasSubscription: props.subscription !== null,
  }),
)

const placeholderText = computed(() => (body.value === 'live' ? null : TILE_BODY_TEXT[body.value]))
const placeholderHint = computed(() => (body.value === 'live' ? null : TILE_BODY_HINT[body.value]))
const connectionHint = computed(() => describeConnectionHint(props.student.connection))
</script>

<template>
  <article
    ref="rootRef"
    class="flex cursor-pointer flex-col overflow-hidden rounded-card border bg-surface shadow-card transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-status-open"
    :class="selected ? 'border-ink/30' : 'border-border-subtle hover:border-ink/20'"
    role="button"
    tabindex="0"
    :aria-current="selected ? 'true' : undefined"
    :aria-label="`放大查看 ${student.displayName} 的桌面画面`"
    data-testid="monitor-tile"
    :data-student-id="student.studentId"
    :data-session-id="student.sessionId ?? ''"
    :data-tile-state="tileState"
    @click="emit('open', $event)"
    @keydown="activate"
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
      <div v-else class="flex h-full flex-col items-center justify-center gap-1 px-4 text-center">
        <p class="text-sm text-ink-muted" data-testid="tile-placeholder">{{ placeholderText }}</p>
        <p v-if="placeholderHint" class="text-xs text-ink-muted/80">{{ placeholderHint }}</p>
      </div>
    </div>

    <footer class="flex items-center justify-between gap-2 border-t border-border-subtle px-4 py-2">
      <span class="text-xs text-ink-muted" data-testid="tile-connection">
        连接：{{ connectionHint }}
      </span>
      <span v-if="student.camera.active" class="text-xs text-ink-muted">摄像头已开启</span>
    </footer>
  </article>
</template>
