<script setup lang="ts">
import type { MonitorStudent } from '@classwatch/shared-types'
import { AppBadge, AppButton } from '@classwatch/ui'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { CAMERA_PIP_TEXT, deriveCameraPipState, isCameraPipVisible } from '../lib/camera-pip.ts'
import type {
  CameraSubscription,
  MicrophoneSubscription,
  ScreenSubscription,
} from '../lib/media/media-room.ts'
import {
  TILE_BODY_HINT,
  TILE_BODY_TEXT,
  describeMonitorBadge,
  describeNetworkLevel,
  describeTileBody,
  type MonitorMediaState,
} from '../lib/monitor-status.ts'
import {
  canStartPrivateTalk,
  describeTalkActionText,
  talkBlockedReason,
} from '../lib/private-talk.ts'
import type { TeacherMicState } from '../lib/teacher-mic.ts'

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
 * 这个组件只做两件事——把已经拿到的轨道挂到 `<video>` 上，以及把 DTO 里的
 * 设备状态如实画出来。
 *
 * Camera 区（§30 右上角）Phase 9 起是**真实画面**：学生开着摄像头时显示订阅到的画面，
 * 没开就说"未开启"，订阅失败就说"订阅失败"——三种情况都是不同的话，
 * 合成一句"暂无画面"会让老师无法判断该不该等。
 *
 * 私密语音（§31）在 Phase 10 真正启用：
 * - 「语音沟通」= POST（切换目标就是再点一次，后端负责撤销旧的）；
 * - 当前目标是这个学生时，按钮变成「正在与 X 语音沟通 · 结束」——高亮是**界面必须
 *   替老师记住的事**：音频是看不见的，切到别的卡片之后再回来，他没法凭记忆知道
 *   自己的麦克风还在对谁广播；
 * - `TEACHER_MIC_REQUIRED` 时给出「请先开启你的麦克风」+ 一键开麦（§31 的可执行提示）；
 * - 未进入课堂的学生（`sessionId === null`）按钮**禁用并写出原因**，而不是让老师
 *   点一个注定被拒绝的按钮。
 *
 * 听学生的麦克风（§32）同样必须写出来："正在听该学生的麦克风"这句话是老师
 * 唯一的线索——耳机里在放什么，他无法从界面上别的地方推断出来。
 */
const props = withDefaults(
  defineProps<{
    student: MonitorStudent
    subscription: ScreenSubscription | null
    mediaState: MonitorMediaState
    /** 摄像头订阅（§24）；null 表示现在没有可播的摄像头画面。 */
    cameraSubscription?: CameraSubscription | null
    cameraMediaState?: MonitorMediaState
    /** 这个学生是不是当前私密语音目标（§31）。 */
    talkTarget?: boolean
    /** 私密语音请求正在进行（按钮 loading）。 */
    talkBusy?: boolean
    /** 上一次私密语音失败的说明（由父组件按"是不是这个学生"过滤后传入）。 */
    talkError?: string | null
    /** 失败原因是"老师还没开麦"（决定要不要给一键开麦入口）。 */
    talkMicRequired?: boolean
    /** 老师自己的麦克风状态（§27）。 */
    teacherMicState?: TeacherMicState
    /** 该学生的麦克风订阅（§32）；null 表示现在没有可听的声音。 */
    microphoneSubscription?: MicrophoneSubscription | null
    microphoneMediaState?: MonitorMediaState
  }>(),
  {
    cameraSubscription: null,
    cameraMediaState: 'none',
    talkTarget: false,
    talkBusy: false,
    talkError: null,
    talkMicRequired: false,
    teacherMicState: 'off',
    microphoneSubscription: null,
    microphoneMediaState: 'none',
  },
)

const emit = defineEmits<{
  close: []
  /** 老师要发起（或切换）与这个学生的私密语音。 */
  startTalk: []
  /** 老师要结束当前私密语音。 */
  stopTalk: []
  /** 老师在"请先开启你的麦克风"提示里点了一键开麦。 */
  enableMic: []
}>()

const videoRef = ref<HTMLVideoElement | null>(null)
const cameraVideoRef = ref<HTMLVideoElement | null>(null)
/** 学生麦克风的 `<audio>`（§32）：隐藏、无控件——老师要听的是声音，不是播放器 UI。 */
const micAudioRef = ref<HTMLAudioElement | null>(null)
/** 面板容器：打开时把焦点移进来（见 onMounted），关闭时由父组件把焦点还给卡片。 */
const panelRef = ref<HTMLElement | null>(null)

let detach: (() => void) | null = null
let detachCamera: (() => void) | null = null
let detachMicrophone: (() => void) | null = null

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

/** Camera 区的 attach：与网格里的画中画是**同一条订阅、不同的元素**（§30）。 */
watch(
  [() => props.cameraSubscription, cameraVideoRef],
  ([subscription]) => {
    detachCamera?.()
    detachCamera = null
    if (subscription === null || cameraVideoRef.value === null) return
    detachCamera = subscription.attach(cameraVideoRef.value)
  },
  { immediate: true, flush: 'post' },
)

/**
 * 学生麦克风的 attach（§32）。
 *
 * WHY 由组件挂而不是由 store 挂：订阅**属于** store 的计划（只订 Focus 那一个），
 * 而"声音放进哪个元素"是视图的事——同一个订阅将来可能被挂到别处（例如一个音量
 * 指示组件）。挂载点是隐藏的 `<audio>`，老师不需要、也不该看到一个播放器控件。
 */
watch(
  [() => props.microphoneSubscription, micAudioRef],
  ([subscription]) => {
    detachMicrophone?.()
    detachMicrophone = null
    if (subscription === null || micAudioRef.value === null) return
    detachMicrophone = subscription.attach(micAudioRef.value)
  },
  { immediate: true, flush: 'post' },
)

onBeforeUnmount(() => {
  detach?.()
  detach = null
  detachCamera?.()
  detachCamera = null
  detachMicrophone?.()
  detachMicrophone = null
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

/**
 * Camera 区（§30 的右上角）：只有 'visible' 才渲染画面，其余三档各说一句话。
 *
 * WHY 不在这里自己发订阅：§30 明确要求"Focus 优先订阅较高质量 Screen Track"，
 * 而订阅集合与画质由 store 的计划统一决定（§52）。组件只消费结果。
 */
const cameraPipState = computed(() =>
  deriveCameraPipState(props.student, {
    state: props.cameraMediaState,
    hasSubscription: props.cameraSubscription !== null,
  }),
)
const cameraVisible = computed(() => isCameraPipVisible(cameraPipState.value))
const cameraText = computed(() =>
  cameraVisible.value
    ? null
    : CAMERA_PIP_TEXT[cameraPipState.value as 'off' | 'waiting' | 'failed'],
)

/* -------------------------------------------------------------------------- */
/* 私密语音（§31）与"正在听谁的麦克风"（§32）                                  */
/* -------------------------------------------------------------------------- */

/** 能不能发起：未进入 / 已离开的学生连按钮都不该可点（原因由下面那句说明给出）。 */
const canTalk = computed(() => canStartPrivateTalk(props.student))

/** 不能发起时那句原因（`canTalk` 为真时为 null）。 */
const talkBlocked = computed(() => talkBlockedReason(props.student))

/** 当前目标：按钮变成"正在与 X 语音沟通 · 结束"（§31 的高亮）。 */
const talkLabel = computed(() =>
  props.talkTarget ? describeTalkActionText(props.student.displayName) : '语音沟通',
)

/**
 * 麦克风听感状态（§32）：三档各说一句话，而且必须一直显示其中一句——
 * 音频是看不见的，面板上不写出来，老师就只能靠猜。
 */
const listeningText = computed(() => {
  if (props.microphoneSubscription !== null) {
    return `正在听${props.student.displayName}的麦克风`
  }
  if (props.microphoneMediaState === 'failed') return '该学生的麦克风订阅失败'
  if (props.microphoneMediaState === 'pending') return '正在连接该学生的麦克风…'
  if (props.student.microphone.active) return '正在连接该学生的麦克风…'
  return '该学生未开启麦克风'
})

/** 已经有声音在放（决定要不要渲染那个隐藏的 `<audio>`）。 */
const hasMicrophoneAudio = computed(() => props.microphoneSubscription !== null)
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
          <!--
            Camera 区（§30 的右上角留位）。Phase 9 起是**真实画面**：
            与网格里的画中画共用同一条订阅（§52 的"同一份下行，多处 attach"），
            所以展开 Focus 不会让 SFU 再推一路视频。
          -->
          <div
            class="aspect-video overflow-hidden rounded-card border border-border-subtle bg-surface-muted"
            data-testid="focus-camera"
          >
            <video
              v-if="cameraVisible"
              ref="cameraVideoRef"
              class="h-full w-full object-cover"
              autoplay
              playsinline
              muted
              data-testid="focus-camera-video"
            />
            <div
              v-else
              class="flex h-full flex-col items-center justify-center gap-1 px-4 text-center"
            >
              <p class="text-sm font-medium text-ink-muted">Camera</p>
              <p class="text-xs text-ink-muted" data-testid="focus-camera-status">
                {{ cameraText }}
              </p>
            </div>
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
              §31 的私密语音：当前目标时按钮变成"正在与 X 语音沟通 · 结束"（高亮），
              否则是「语音沟通」。切换目标 = 对另一个学生再点一次，后端会撤销旧的订阅，
              界面只需要跟着 `talkTarget` 走。
            -->
            <AppButton
              v-if="talkTarget"
              block
              data-testid="focus-talk"
              :loading="talkBusy"
              :disabled="talkBusy"
              @click="emit('stopTalk')"
            >
              {{ talkLabel }}
            </AppButton>
            <AppButton
              v-else
              variant="secondary"
              block
              data-testid="focus-talk"
              :loading="talkBusy"
              :disabled="talkBusy || !canTalk"
              @click="emit('startTalk')"
            >
              {{ talkLabel }}
            </AppButton>

            <!-- 未进入 / 已离开：按钮禁用，并**在这里**说明原因（不要点了才报错）。 -->
            <p
              v-if="!talkTarget && talkBlocked"
              class="mt-2 text-xs text-ink-muted"
              data-testid="focus-talk-blocked"
            >
              {{ talkBlocked }}
            </p>

            <!--
              §31 的可执行提示：`TEACHER_MIC_REQUIRED` 时不能只说"失败了"，
              必须给一个真的能解决问题的按钮。
            -->
            <div
              v-if="talkMicRequired"
              class="mt-3 space-y-2"
              data-testid="focus-talk-mic-required"
            >
              <p class="text-xs text-ink-muted">请先开启你的麦克风，再发起语音沟通。</p>
              <AppButton
                size="sm"
                data-testid="focus-talk-enable-mic"
                :loading="teacherMicState === 'requesting'"
                :disabled="teacherMicState === 'requesting' || teacherMicState === 'on'"
                @click="emit('enableMic')"
              >
                🎤 开启麦克风
              </AppButton>
            </div>
            <!-- 其余失败（学生不在课堂 / 不是 owner / 课堂已结束）只给一句可读的话。 -->
            <p
              v-else-if="talkError"
              class="mt-2 text-xs text-status-danger"
              data-testid="focus-talk-error"
            >
              {{ talkError }}
            </p>

            <!--
              §32：老师必须知道自己的耳机里在放什么。这一行**永远**在，而且三档都说清楚
              （正在听 / 正在连接 / 对方没开麦）——音频没有画面，"什么都没显示"是最糟的状态。
            -->
            <p class="mt-3 text-xs text-ink-muted" data-testid="focus-listening">
              {{ listeningText }}
            </p>
            <!-- 有订阅就渲染：隐藏的 <audio> 是真正出声的地方，没有它"正在听"就是一句空话。 -->
            <audio
              v-if="hasMicrophoneAudio"
              ref="micAudioRef"
              autoplay
              data-testid="focus-mic-audio"
            />
          </div>
        </aside>
      </div>
    </section>
  </div>
</template>
