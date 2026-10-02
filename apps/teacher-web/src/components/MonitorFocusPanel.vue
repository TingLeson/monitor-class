<script setup lang="ts">
import type { MonitorStudent } from '@classwatch/shared-types'
import { AppBadge, AppButton } from '@classwatch/ui'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { ScreenSubscription } from '../lib/media/media-room.ts'
import {
  TILE_BODY_HINT,
  TILE_BODY_TEXT,
  describeMonitorBadge,
  describeNetworkLevel,
  describeTileBody,
  type MonitorMediaState,
} from '../lib/monitor-status.ts'

/**
 * Focus View（§30）—— 一个学生的"大画面 + 设备面板"。
 *
 * WHY 它不是路由（任务书的决策）：监督墙的价值在于**连续盯**，点击卡片只是把注意力
 * 收窄到一个人身上，老师随时会点回网格。做成路由会带来三件坏事：
 * 浏览器后退变成"退出 Focus"这种难以预期的行为；每次点击都要重新挂载视图、
 * 重新订阅；URL 上多一个"打开着谁的画面"的地址，刷新或分享时语义含糊。
 * 所以它是一层页面内的状态，用 `role="dialog"` + Esc + 焦点归还来保证可访问性。
 *
 * 订阅不在这里发起：画质与订阅集合由 store 的计划决定（§30 / §52）。
 * 这个组件只做两件事——把已经拿到的轨道挂到大 `<video>` 上，以及把 DTO 里的
 * 设备状态如实画出来。
 *
 * 摄像头预览（§30 右上角的 Camera 区）与私密语音（§31）分别属于 Phase 9 / 10，
 * 这里给出的是**明确的占位说明**，不是能点但没反应的假控件。
 */
const props = defineProps<{
  student: MonitorStudent
  subscription: ScreenSubscription | null
  mediaState: MonitorMediaState
}>()

const emit = defineEmits<{ close: [] }>()

const videoRef = ref<HTMLVideoElement | null>(null)
/** 面板容器：打开时把焦点移进来（见 onMounted），关闭时由父组件把焦点还给卡片。 */
const panelRef = ref<HTMLElement | null>(null)

let detach: (() => void) | null = null

watch(
  [() => props.subscription, videoRef],
  ([subscription]) => {
    detach?.()
    detach = null
    if (subscription === null || videoRef.value === null) return
    detach = subscription.attach(videoRef.value)
  },
  { immediate: true, flush: 'post' },
)

onBeforeUnmount(() => {
  detach?.()
  detach = null
})

/**
 * Esc 退出（任务书明确要求的键盘出口）。
 *
 * 监听挂在 `window` 而不是面板上：老师点开 Focus 之后接着用鼠标在别处划过、
 * 或用 Tab 把焦点移到面板外，按键事件都不会再冒泡到面板里，
 * 那样 Esc 就会时灵时不灵——一个"大多数时候能用"的退出键比没有更糟。
 *
 * 只处理 Escape：其余按键一律放行，免得抢掉老师正在用的浏览器快捷键。
 */
function onKeydown(event: KeyboardEvent): void {
  if (event.key !== 'Escape') return
  event.stopPropagation()
  emit('close')
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  /**
   * 打开就把焦点移进面板：不这样做，键盘与读屏用户还停在网格上，
   * 而屏幕上已经换成另一个视图了。焦点移到容器（tabindex="-1"）而不是某个按钮上，
   * 下一次 Tab 会自然落到"返回网格"——这是非模态对话框的常规做法。
   * 焦点归还由父组件负责（它知道是哪张卡片打开的）。
   */
  panelRef.value?.focus()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
})

const badge = computed(() => describeMonitorBadge(props.student))

/** 与网格卡片用**同一个**合成规则：换个尺寸不该换一套状态口径（§51）。 */
const body = computed(() =>
  describeTileBody(props.student, {
    state: props.mediaState,
    hasSubscription: props.subscription !== null,
  }),
)

const placeholderText = computed(() => (body.value === 'live' ? null : TILE_BODY_TEXT[body.value]))
const placeholderHint = computed(() => (body.value === 'live' ? null : TILE_BODY_HINT[body.value]))

const network = computed(() => describeNetworkLevel(props.student.connection))

/**
 * 设备行（§30 的 Screen / Camera / Mic）。
 *
 * 取值全部来自 Monitor DTO：界面上"麦克风开着"必须是后端观测到的事实，
 * 而不是从媒体轨道猜的（老师端这一 Phase 根本不订阅音频，猜不出来）。
 */
const devices = computed(() => [
  { key: 'screen', label: 'Screen', active: props.student.screen.active },
  { key: 'camera', label: 'Camera', active: props.student.camera.active },
  { key: 'microphone', label: 'Mic', active: props.student.microphone.active },
])
</script>

<template>
  <div
    class="fixed inset-0 z-50 overflow-y-auto overscroll-contain bg-surface-muted/95 p-4 backdrop-blur-sm sm:p-6"
    data-testid="monitor-focus"
  >
    <section
      ref="panelRef"
      role="dialog"
      aria-modal="false"
      tabindex="-1"
      :aria-label="`聚焦视图：${student.displayName}`"
      class="mx-auto flex max-w-7xl flex-col gap-4 focus:outline-none"
    >
      <header class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex flex-wrap items-center gap-x-4 gap-y-1">
          <h2 class="text-xl font-semibold tracking-tight" data-testid="focus-name">
            {{ student.displayName }}
          </h2>
          <AppBadge
            :label="`${badge.emoji} ${badge.label}`"
            :tone="badge.tone"
            data-testid="focus-badge"
          />
          <span class="text-xs text-ink-muted" data-testid="focus-hint">按 Esc 返回网格</span>
        </div>
        <AppButton
          variant="secondary"
          size="sm"
          aria-label="关闭聚焦视图，返回网格"
          data-testid="focus-close"
          @click="emit('close')"
        >
          返回网格
        </AppButton>
      </header>

      <div class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_18rem]">
        <!--
          大画面：同一份订阅、更大的元素。
          LiveKit 的自适应尺寸取**已挂载元素里最大**的那个，所以 Focus 打开时
          这条订阅自然升到更高的层；关闭后网格里的小窗还在，画面不会闪一下。
        -->
        <div
          class="aspect-video overflow-hidden rounded-card border border-border-subtle bg-surface-muted shadow-card"
        >
          <video
            v-if="body === 'live'"
            ref="videoRef"
            class="h-full w-full object-contain"
            autoplay
            playsinline
            muted
            data-testid="focus-video"
          />
          <div
            v-else
            class="flex h-full flex-col items-center justify-center gap-2 px-6 text-center"
          >
            <p class="text-sm font-medium text-ink" data-testid="focus-placeholder">
              {{ placeholderText }}
            </p>
            <p v-if="placeholderHint" class="text-sm text-ink-muted">{{ placeholderHint }}</p>
          </div>
        </div>

        <aside class="flex flex-col gap-4">
          <!-- Camera：§30 的信息面板顶部留位，Phase 9 接入。 -->
          <div
            class="flex aspect-video flex-col items-center justify-center gap-1 rounded-card border border-dashed border-border-subtle bg-surface px-4 text-center"
            data-testid="focus-camera-placeholder"
          >
            <p class="text-sm font-medium text-ink-muted">Camera</p>
            <p class="text-xs text-ink-muted">摄像头画面将在 Phase 9 接入</p>
          </div>

          <div class="rounded-card border border-border-subtle bg-surface px-4 py-3 shadow-card">
            <dl class="space-y-2">
              <div
                v-for="device in devices"
                :key="device.key"
                class="flex items-center justify-between gap-3 text-sm"
                :data-testid="`focus-device-${device.key}`"
              >
                <dt class="text-ink-muted">{{ device.label }}</dt>
                <dd class="flex items-center gap-1.5 font-medium text-ink">
                  <span
                    class="h-1.5 w-1.5 rounded-full"
                    :class="device.active ? 'bg-status-open' : 'bg-status-neutral'"
                    aria-hidden="true"
                  />
                  {{ device.active ? '已开启' : '未开启' }}
                </dd>
              </div>
              <div
                class="flex items-center justify-between gap-3 text-sm"
                data-testid="focus-network"
              >
                <dt class="text-ink-muted">Network</dt>
                <dd class="font-medium text-ink">{{ network }}</dd>
              </div>
            </dl>
          </div>

          <div class="rounded-card border border-border-subtle bg-surface px-4 py-3 shadow-card">
            <!--
              §31 的私密语音属于 Phase 10。按钮**禁用**并写出原因：
              一个能点但没反应的按钮会让老师在课堂上以为是自己操作错了。
            -->
            <AppButton variant="secondary" disabled block data-testid="focus-talk">
              语音沟通
            </AppButton>
            <p class="mt-2 text-xs text-ink-muted" data-testid="focus-talk-note">
              Phase 10 接入：私密语音将在后续版本开放。
            </p>
          </div>
        </aside>
      </div>
    </section>
  </div>
</template>
