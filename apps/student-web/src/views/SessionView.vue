<script setup lang="ts">
import { AppAlert, AppButton, AppCard, AppEmptyState, StatusDot } from '@classwatch/ui'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { describeConnectionQuality } from '../lib/connection-quality'
import { describeSessionPhase } from '../lib/media-session-state.ts'
import { PRIVATE_TALK_DECLINE_NOTE } from '../lib/private-talk.ts'
import { describeRealtimeStatus } from '../lib/realtime-status.ts'
import { describeScreenGateState } from '../lib/screen-capture-messages'
import { useMediaSessionStore } from '../stores/media-session.ts'
import { useRealtimeStore } from '../stores/realtime.ts'
import { useScreenShareStore } from '../stores/screen-share'

/**
 * 课堂会话页（§55 / §56 / §22 / §49）—— Phase 6 的真实实现，Phase 9 追加摄像头，
 * Phase 10 追加麦克风与私密语音提示。
 *
 * 这一页只做五件事，别的一概不做：
 *
 * 1. **接收** PreJoin 交接过来的凭据与那条已经授权的屏幕轨道（§18 的顺序：
 *    先 Gate，后 join），连上 LiveKit 并把它发布成 ScreenShare（§20）。
 * 2. **只显示状态**（§56）：正在共享整屏 / 摄像头与麦克风是否启用 / 网络质量 /
 *    当前状态。**没有**自己的屏幕预览——那会形成 screen inside screen inside
 *    screen，而且对"我到底有没有被看见"这个问题毫无帮助（页面上那句
 *    「正在共享整个屏幕」才是答案）。
 *    **例外是摄像头自视小窗**（Phase 9，§24）：理由见 store 里 `cameraStream` 的说明——
 *    学生必须能确认自己在画面里（角度、光线、开错设备都只能靠眼睛发现），
 *    而屏幕共享不存在这个问题（共享的是整块显示器，没有"取景"可言）。
 * 3. **在共享中断时给出一条恢复路径**（§22）：提示 + 「重新共享整个屏幕」，
 *    重新走一遍**完整**的 Gate（含 displaySurface 检查），然后发布新轨道。
 *    这里**不**重新 join——会话还在，只是轨道没了；重连是媒体链路的另一条路径。
 * 4. **在课堂被老师关闭时收尾**（§49）：收到 `ROOM_CLOSED`（§47 的实时事件）立刻
 *    断开媒体、停止捕获、回课堂列表。这里还保留了低频兜底轮询（见 store）：
 *    实时通道断线期间，学生仍必须能发现课堂已经关闭。
 * 5. **如实显示实时通道状态**（§47）：连不上时明说"正在重连 / 可能不是最新"。
 *    这一页显示的是"老师能不能看到我"，界面假装一切正常是这里最不能犯的错。
 *
 * Phase 10 追加的两块（§25/§31）都在同一个原则下：
 * - `🎤 开启麦克风` 是**真开关**（点击才 `getUserMedia`），开/关/再开各自独立；
 *   老师发起的请求只显示提示与两个按钮——**不能**替他点"开启麦克风"。
 * - 老师的私密语音只用两种形态出现：一个"要不要开麦"的请求卡片，和一条
 *   "正在与 X 语音沟通"的持续指示。两种形态都**不提**任何其他学生（§26）。
 *
 * 刷新页面的行为是刻意设计的：token 只存在内存里，刷新即丢失。页面因此显示
 * 「会话信息已丢失」并给回列表的入口——而不是拿一个过期的 token 去反复重连，
 * 那只会让学生盯着一个永远连不上的"连接中…"。
 */
const route = useRoute()
const router = useRouter()
const session = useMediaSessionStore()
const screenShare = useScreenShareStore()
const realtime = useRealtimeStore()

/**
 * 老师关闭课堂后自动回列表的延迟。
 *
 * WHY 需要这个延迟而不是立刻跳走：§49 要求先给出「老师已经关闭本课堂」这句提示。
 * 直接跳转等于把提示丢进一个已经不存在的页面——学生只会看到自己莫名其妙回到了
 * 列表页。按钮同时提供，性子急的学生不必等。
 */
const CLOSED_REDIRECT_MS = 4000

const closedRedirecting = ref(false)
let closedTimer: ReturnType<typeof setTimeout> | null = null

/** 会话 id（来自 join 响应）。与地址栏不一致时视图会同步过去（§50）。 */
const sessionId = computed(() => session.sessionId)

/** 课堂名。凭据丢失或首次轮询还没回来时为空，此时不显示一个假的标题。 */
const classroomName = computed(() => session.classroom?.name ?? null)
const teacherName = computed(() => session.classroom?.teacher.displayName ?? null)

/** §56 的「网络 ● Good」：五档中文 + 语义色。 */
const qualityDisplay = computed(() => describeConnectionQuality(session.quality))

/** §56 的「当前状态：…」：已进入课堂 / 连接中… / ⚠ 已停止屏幕共享。 */
const phaseDisplay = computed(() => describeSessionPhase(session.phase))

/**
 * 实时通道状态（§47）。
 *
 * 这一行不是装饰：它回答的是"我现在看到的课堂状态有多新"。断了就必须说出来，
 * 因为兜底轮询最多要 30–60 秒才会发现"老师关了课堂"。
 */
const realtimeDisplay = computed(() =>
  describeRealtimeStatus(realtime.state, {
    authFailed: realtime.authFailed,
    wasConnected: realtime.wasConnected,
  }),
)

/**
 * Gate / 能力自检的说明（重新共享失败时要就地显示原因）。
 *
 * 与 PreJoin 共用同一个折叠函数，理由见 `screen-capture-messages.ts`：
 * 学生在这两处遇到的是同一件事，话术必须一致。
 *
 * `isLost` 传 false 是**有意**的：进入本页之后，轨道结束由会话 store 接管
 * （screen-share store 的所有权已在 PreJoin 移交时释放），"已停止屏幕共享"
 * 由下面那张 §22 卡片专门表达，不该在同一屏里出现两遍。
 */
const gateMessage = computed(() =>
  describeScreenGateState({
    isLost: false,
    unsupportedReason: screenShare.unsupportedReason,
    failure: screenShare.failure,
  }),
)

/** 正在重新共享：按钮 loading + 一句说明（§22 的恢复过程要可见）。 */
const resharing = computed(() => screenShare.isRequesting)

/* -------------------------------------------------------------------------- */
/* 摄像头自视画面（§24 / §56 的唯一例外）                                      */
/* -------------------------------------------------------------------------- */

/**
 * 自视小窗的 `<video>`。
 *
 * WHY 直接绑 `srcObject` 而不是走媒体层：这条流是**本地**的（还没经过任何 SFU），
 * 学生要看的就是"摄像头到底拍到了什么"。走订阅链路既没有意义（没有人会订阅自己），
 * 也会把 §26 的"学生端不订阅任何轨道"这条不变量弄出一个例外。
 *
 * `flush: 'post'`：`<video>` 是 `v-if` 出来的，只有 DOM 更新之后 ref 才有值。
 */
const cameraVideoRef = ref<HTMLVideoElement | null>(null)

watch(
  [() => session.cameraStream, cameraVideoRef],
  ([stream]) => {
    const element = cameraVideoRef.value
    if (element === null) return
    element.srcObject = stream
  },
  { immediate: true, flush: 'post' },
)

/** §24：入口只在**进入课堂之后**出现；没进课堂时连按钮都不该有。 */
const canToggleCamera = computed(() => session.canUseCamera)

/** §25：麦克风与摄像头同一条规则——进入课堂之后才显示入口（点击才申请权限）。 */
const canToggleMicrophone = computed(() => session.canUseMicrophone)

/** 麦克风的一行状态文字（与"正在共享整个屏幕"是两件独立的事）。 */
const microphoneStatusText = computed(() => {
  switch (session.micState) {
    case 'on':
      return session.micMuted ? '已开启（已静音）' : '已开启'
    case 'requesting':
      return '正在请求麦克风…'
    case 'error':
      return '未开启'
    default:
      return '未开启'
  }
})

/**
 * §25：「暂不开启」之后必须说明"老师仍能单向讲话"。
 *
 * 显示条件是"沟通在进行、而老师听不到我"，并且**已经回答过**（提示卡片不在了）：
 * 提示卡片在场时它自己已经解释过"开启麦克风后老师才能听到你"，两句话说两遍
 * 只会把真正重要的那一句淹掉。回答之后（拒绝或静音）就轮到这句话值班了。
 */
const showOneWayNote = computed(
  () => session.isTalkingWithTeacher && !session.hasTalkRequest && !session.isMicrophoneAudible,
)

/** 摄像头的一行状态文字（与"正在共享整个屏幕"是两件独立的事）。 */
const cameraStatusText = computed(() => {
  switch (session.cameraState) {
    case 'on':
      return '已开启'
    case 'requesting':
      return '正在请求摄像头…'
    case 'error':
      return '未开启'
    default:
      return '未开启'
  }
})

/**
 * 进入本页时开始会话。
 *
 * 顺序：`begin()`（连接 → 发布）→ store 自己开始低频轮询课堂状态（§49）。
 * 失败不会被抛出来：store 把失败记录成 `media-error` + 一段中文文案，
 * 页面负责渲染并提供重试入口。
 */
onMounted(() => {
  void session.begin()
  /**
   * 页面卸载/关闭时**尽力**断开（§22 / §65 Case 14）。
   *
   * `beforeunload` 里只能做同步操作：此时发起异步请求，浏览器通常会在请求完成
   * 前销毁页面（fetch 被取消），因此这里只停本地捕获 + 发起断开，不做上报。
   * 服务端最终通过 LiveKit 的 participant_left / track_unpublished 发现学生离线
   * （§45/§46：不要只相信前端主动报告）。
   */
  window.addEventListener('beforeunload', releaseOnUnload)
})

/** 见 onMounted 的说明：这里必须是同步的，所以只调用 store 的同步兜底。 */
function releaseOnUnload(): void {
  session.releaseNow()
}

onBeforeUnmount(() => {
  window.removeEventListener('beforeunload', releaseOnUnload)
  if (closedTimer !== null) {
    clearTimeout(closedTimer)
    closedTimer = null
  }
  /**
   * 离开页面（路由跳转、组件卸载）时结束会话：上报 leave + 断开媒体 + 停止捕获。
   * 不 await：卸载路径上没人能等，而且 `leave()` 内部已经把网络上报限制在
   * 1.5 秒内（见 store 的 settleWithin），本地清理则是同步完成的。
   */
  void session.leave()
})

/**
 * 重试（媒体连接失败 / token 过期 §43）。
 *
 * store 会**重新 join** 拿一份新 token：旧 token 是短时的，失败原因很可能正是
 * 它已经过期；复用旧凭据重试只会再失败一次。
 */
function retryMedia(): void {
  void session.retry()
}

/**
 * 重新共享整个屏幕（§22）。
 *
 * 三步，顺序不能变：
 * 1. 重新走 Phase 5 的完整 Gate（能力自检 → getDisplayMedia → displaySurface
 *    必须是 monitor）。**不存在**"上次通过过就一直放行"的捷径；
 * 2. 把新轨道从 screen-share store 手里接过来（所有权移交，不停轨道）；
 * 3. 直接 publish。**不重新 join**：会话、身份、权限都没变，变的只是我这边
 *    多了一条轨道；重新 join 会让老师那端的学生卡片闪断一次，而这对恢复画面
 *    没有任何帮助。
 */
async function reshareScreen(): Promise<void> {
  if (resharing.value) return
  // 必须在点击处理函数里同步发起：getDisplayMedia 需要用户手势（transient activation）。
  await screenShare.start()
  if (!screenShare.isSharing) return
  const next = screenShare.handOff()
  if (next === null) return
  await session.publishCapture(next)
}

/** 主动离开：store 负责上报与断开，然后回课堂列表（§49 的出口）。 */
async function leaveClassroom(): Promise<void> {
  await session.leave()
  await router.push({ name: 'student-classrooms' })
}

/**
 * 老师关闭课堂（§49）：提示 → 自动回列表。
 *
 * 断开媒体与停止捕获已经由 store 在发现 CLOSED 的那一刻同步做完了——
 * 界面这里的延迟只是为了让那句提示能被看见。
 */
watch(
  () => session.isClosedByTeacher,
  (closed) => {
    if (!closed || closedTimer !== null) return
    closedRedirecting.value = true
    closedTimer = setTimeout(() => {
      void router.push({ name: 'student-classrooms' })
    }, CLOSED_REDIRECT_MS)
  },
)

/**
 * 会话 id 变化（重试后后端可能给出新 session，§50「新连接取代旧连接」）时把地址栏
 * 同步过去，避免"页面上的会话"和"地址栏里的会话"是两个东西。
 */
watch(sessionId, (next) => {
  const current = String(route.params.sessionId ?? '')
  if (next === null || next === '' || next === current) return
  void router.replace({ name: 'student-session', params: { sessionId: next } })
})
</script>

<template>
  <div class="mx-auto flex max-w-3xl flex-col gap-6">
    <!--
      凭据丢失：刷新页面、直接输入 URL、或从别的标签页复制过来。
      token 只在内存里（§44），所以这不是故障，而是**设计好的**结果：
      给一句解释和一条明确的回头路，绝不显示一个连不上的"连接中…"。
    -->
    <AppCard v-if="session.hasNoSession" data-testid="session-missing">
      <AppEmptyState
        title="会话信息已丢失"
        description="课堂凭据只保存在当前页面里，刷新或重新打开页面后需要重新进入课堂。"
      >
        <RouterLink
          :to="{ name: 'student-classrooms' }"
          class="inline-flex h-8 items-center justify-center rounded-control border border-border-subtle bg-surface px-3 text-xs font-medium text-ink transition-colors hover:bg-surface-muted"
          data-testid="back-to-classrooms"
        >
          返回我的课堂
        </RouterLink>
      </AppEmptyState>
    </AppCard>

    <template v-else>
      <header class="space-y-1">
        <h1 class="text-2xl font-semibold tracking-tight" data-testid="session-classroom-name">
          {{ classroomName ?? '课堂进行中' }}
        </h1>
        <p v-if="teacherName" class="text-sm text-ink-muted" data-testid="session-teacher">
          {{ teacherName }}
        </p>
      </header>

      <!--
        §49：老师关闭了课堂。媒体已经断开、本地捕获已经停止（store 负责），
        这里只负责把话说清楚 + 把学生送回列表。
      -->
      <AppAlert
        v-if="session.isClosedByTeacher"
        tone="info"
        title="老师已经关闭本课堂"
        data-testid="classroom-closed-notice"
      >
        本课堂已经结束，屏幕共享已停止。
        {{ closedRedirecting ? '正在返回我的课堂…' : '点击下面的按钮返回我的课堂。' }}
      </AppAlert>

      <!-- §56 的状态面板：只有状态指示，**没有**自己的画面预览。 -->
      <AppCard data-testid="session-status-panel">
        <div class="space-y-5">
          <!--
            屏幕状态有三种，不是两种：本地 publish 成功但服务端还没确认（§46）时，
            说"正在共享整个屏幕"是在替老师那边打包票。等待确认要说出来。
          -->
          <p
            v-if="session.isOnline && !session.awaitingScreenRestore"
            class="text-base font-medium"
            data-testid="screen-sharing-status"
          >
            🖥 正在共享整个屏幕
          </p>
          <p
            v-else-if="session.isOnline"
            class="text-base font-medium text-ink-muted"
            data-testid="screen-restore-pending"
          >
            🖥 已重新共享，正在等待课堂确认…
          </p>
          <p
            v-else-if="session.isScreenLost"
            class="text-base font-medium text-status-danger"
            data-testid="screen-lost-status"
          >
            ⚠ 已停止屏幕共享
          </p>
          <p v-else class="text-base font-medium text-ink-muted" data-testid="screen-idle-status">
            🖥 正在准备共享整个屏幕
          </p>

          <dl class="grid gap-3 border-t border-border-subtle pt-4 text-sm sm:grid-cols-3">
            <!--
              摄像头（§24）：进入课堂之后才有的**真实开关**，点一下才申请设备权限。
              它与上面那行屏幕状态是两件独立的事：摄像头开着不代表屏幕还在共享，
              反之亦然——所以这里既不改那句话，也不受它影响。
            -->
            <div class="space-y-1" data-testid="camera-row">
              <dt class="text-xs tracking-wide text-ink-muted uppercase">摄像头</dt>
              <dd class="flex flex-wrap items-center gap-2 text-ink">
                <AppButton
                  v-if="canToggleCamera"
                  size="sm"
                  variant="secondary"
                  :loading="session.isCameraRequesting"
                  :disabled="session.isCameraRequesting"
                  data-testid="toggle-camera"
                  @click="session.toggleCamera()"
                >
                  {{ session.cameraActionLabel }}
                </AppButton>
                <!-- 还没进入课堂：入口不该出现（§24 明令禁止在此之前请求摄像头）。 -->
                <span v-else class="text-ink-muted">未启用</span>
                <span data-testid="camera-status">{{ cameraStatusText }}</span>
                <!--
                  §24 的自视小窗（约 120px）。**只有摄像头**：屏幕预览依旧禁止（§56），
                  否则就是 screen inside screen inside screen，而摄像头没有替代方案——
                  学生必须能亲眼确认自己在画面里。
                -->
                <video
                  v-if="session.isCameraOn"
                  ref="cameraVideoRef"
                  class="h-[90px] w-[120px] rounded-control border border-border-subtle bg-surface-muted object-cover"
                  autoplay
                  playsinline
                  muted
                  data-testid="camera-self-view"
                />
              </dd>
            </div>
            <div class="space-y-1" data-testid="microphone-row">
              <dt class="text-xs tracking-wide text-ink-muted uppercase">麦克风</dt>
              <dd class="flex flex-wrap items-center gap-2 text-ink">
                <!--
                  §25：与摄像头逐字同构的开关——**点击才** getUserMedia，
                  开 / 关 / 再开 各自是一次独立的设备请求。入口只在课堂里出现。
                -->
                <AppButton
                  v-if="canToggleMicrophone"
                  size="sm"
                  variant="secondary"
                  :loading="session.isMicRequesting"
                  :disabled="session.isMicRequesting"
                  data-testid="toggle-microphone"
                  @click="session.toggleMicrophone()"
                >
                  {{ session.micActionLabel }}
                </AppButton>
                <span v-else class="text-ink-muted">未启用</span>
                <span data-testid="microphone-status">{{ microphoneStatusText }}</span>
              </dd>
            </div>
            <div class="space-y-1" data-testid="network-row">
              <dt class="text-xs tracking-wide text-ink-muted uppercase">网络</dt>
              <dd>
                <StatusDot
                  :status="qualityDisplay.tone"
                  :label="qualityDisplay.label"
                  data-testid="connection-quality"
                />
              </dd>
            </div>
          </dl>

          <p class="border-t border-border-subtle pt-4 text-sm" data-testid="session-phase">
            当前状态：{{ phaseDisplay.label }}
          </p>

          <p
            v-if="session.reconnecting"
            class="text-xs leading-relaxed text-ink-muted"
            data-testid="session-reconnecting"
          >
            网络不稳定，正在自动重连…
          </p>

          <!--
            实时通道状态（§47）。放在状态面板的最下面一行，措辞中性：
            通道抖动是常态，学生正在共享整块屏幕，这里不该出现吓人的红色告警；
            但也**不能**什么都不说——见 realtimeDisplay 的说明。
          -->
          <div
            class="space-y-2 border-t border-border-subtle pt-4 text-xs leading-relaxed text-ink-muted"
            data-testid="realtime-status"
            :data-realtime-state="realtime.state"
          >
            <p class="flex flex-wrap items-center gap-x-2">
              <StatusDot :status="realtimeDisplay.tone" :label="realtimeDisplay.label" />
              <span v-if="realtimeDisplay.hint">{{ realtimeDisplay.hint }}</span>
            </p>
            <!--
              自动重连已经停止（连续多次连不上）时才给按钮：
              不然这个按钮会变成一个"点了也没用"的装饰。
            -->
            <AppButton
              v-if="realtime.authFailed"
              size="sm"
              variant="secondary"
              data-testid="realtime-retry"
              @click="realtime.restart()"
            >
              重试实时连接
            </AppButton>
          </div>
        </div>
      </AppCard>

      <!--
        §25：老师的私密语音请求。提示里只说**老师是谁**与两个动作——没有任何
        其他学生的信息（§26），也没有"课堂里还有谁在听"这种问题（§31：其他人不接收）。
      -->
      <AppCard v-if="session.hasTalkRequest" data-testid="private-talk-request">
        <div class="space-y-3">
          <p class="text-base font-medium" data-testid="private-talk-request-title">
            {{ session.talkRequestTitle }}
          </p>
          <div class="flex flex-wrap gap-2">
            <!--
              「开启麦克风」必须走完整的一次 getUserMedia：§25 明确老师不能绕过
              浏览器权限远程开麦，所以这个按钮是**唯一**能打开麦克风的入口之一
              （另一个是上面那个开关）。它不会在收到事件时被自动触发。
            -->
            <AppButton
              size="sm"
              :loading="session.isMicRequesting"
              :disabled="session.isMicRequesting"
              data-testid="private-talk-accept"
              @click="session.acceptTalkRequest()"
            >
              开启麦克风
            </AppButton>
            <AppButton
              size="sm"
              variant="secondary"
              data-testid="private-talk-decline"
              @click="session.declineTalk()"
            >
              暂不开启
            </AppButton>
          </div>
          <p class="text-xs leading-relaxed text-ink-muted">
            开启麦克风后老师才能听到你。拒绝不会让你退出课堂，也不会停止屏幕共享。
          </p>
        </div>
      </AppCard>

      <!--
        §25/§31：老师正在与我语音沟通的**持续指示**。它不随提示关闭而消失——
        因为老师仍然可以单向讲话（`showOneWayNote` 就是为这件事准备的那句话）。
      -->
      <AppCard v-if="session.isTalkingWithTeacher" data-testid="private-talk-active">
        <div class="space-y-3">
          <p class="text-base font-medium" data-testid="private-talk-active-text">
            {{ session.talkActiveText }}
          </p>
          <!-- 开着麦克风时给一个"静音自己"的入口（§31：本地静音即可）。 -->
          <AppButton
            v-if="session.isMicOn"
            size="sm"
            variant="secondary"
            data-testid="private-talk-self-mute"
            @click="session.toggleSelfMute()"
          >
            {{ session.micMuted ? '取消静音' : '静音自己' }}
          </AppButton>
          <p
            v-if="showOneWayNote"
            class="text-sm leading-relaxed text-ink-muted"
            data-testid="private-talk-one-way-note"
          >
            {{ PRIVATE_TALK_DECLINE_NOTE }}
          </p>
          <!--
            音频被自动播放策略拦住时**必须说出来**：界面显示"正在与老师语音沟通"、
            学生却什么都听不到，是本 Phase 最严重的一种界面撒谎。点击是浏览器要求的
            用户手势，所以恢复入口只能是一个按钮。
          -->
          <div
            v-if="session.teacherAudio === 'blocked'"
            class="space-y-2"
            data-testid="private-talk-audio-blocked"
          >
            <p class="text-sm leading-relaxed text-ink-muted">
              老师的语音已经就绪，但浏览器要求你先点击一次才能播放声音。
            </p>
            <AppButton
              size="sm"
              data-testid="private-talk-resume-audio"
              @click="session.resumeTeacherAudio()"
            >
              点击播放声音
            </AppButton>
          </div>
        </div>
      </AppCard>

      <!--
        §22：屏幕共享中断。这不是"错误"，而是"当前状态不满足课堂要求"，
        因此给出 ⚠ + 一个明确的恢复动作，并且**留在本页**——跳走或伪装成功
        都会在老师那端留下一个没有屏幕的"在线"学生。
      -->
      <AppCard v-if="session.isScreenLost" data-testid="screen-lost">
        <div class="space-y-2">
          <h2 class="text-base font-medium">⚠ 已停止屏幕共享</h2>
          <p class="text-sm leading-relaxed text-ink-muted">
            当前课堂要求持续共享整个屏幕。重新共享时会重新检查你选择的是不是整块显示器。
          </p>
          <!--
            服务端判定丢失、而本地这条轨道可能还活着（§46 的双通道本来就可能不同步）。
            这时要把"以课堂侧为准"说出来，否则学生看着系统的共享提示会以为界面坏了。
          -->
          <p
            v-if="session.screenLostByServer"
            class="text-sm leading-relaxed text-ink-muted"
            data-testid="screen-lost-server-notice"
          >
            课堂侧已经确认没有收到你的屏幕画面，请重新共享一次。
          </p>
        </div>

        <div class="mt-4">
          <AppButton
            :loading="resharing"
            :disabled="resharing"
            data-testid="reshare-screen"
            @click="reshareScreen"
          >
            重新共享整个屏幕
          </AppButton>
        </div>
      </AppCard>

      <!-- 重新共享时 Gate 又会拒绝一次（选了窗口 / 取消了）：就地说明原因。 -->
      <AppAlert
        v-if="gateMessage"
        tone="danger"
        :title="gateMessage.title"
        data-testid="screen-gate-error"
      >
        {{ gateMessage.description }}
      </AppAlert>

      <!--
        摄像头失败（§24）。刻意用 **info** 而不是 danger，也不叫"错误"：
        摄像头是可选设备（§21），它打不开与"课堂出问题了"完全是两回事。
        用红色告警会把学生吓到以为自己上不了课，也会把上面那条真正的
        「⚠ 已停止屏幕共享」淹没掉。文案本身来自词表，已包含下一步动作。
      -->
      <AppAlert
        v-if="session.hasCameraFailure"
        tone="info"
        title="摄像头没有开启（不影响上课）"
        data-testid="camera-failure"
      >
        {{ session.cameraFailure?.message }}
      </AppAlert>

      <!--
        麦克风失败（§25）。与摄像头用**完全一样**的呈现方式：info 色调、
        标题里明说"不影响上课"。文案来自词表，已包含下一步动作，并且说清
        "仍然能听到老师讲话"——否则学生会以为拒绝开麦就等于整段沟通结束。
      -->
      <AppAlert
        v-if="session.hasMicFailure"
        tone="info"
        title="麦克风没有开启（不影响上课）"
        data-testid="microphone-failure"
      >
        {{ session.micFailure?.message }}
      </AppAlert>

      <!--
        媒体链路失败（连不上 / 发布失败 / 连接中断）。
        文案来自 store 的词表，**不含**任何 SDK 报文、地址或凭据（§44 / §58）。
      -->
      <AppAlert
        v-if="session.hasFailure"
        tone="danger"
        title="媒体连接有问题"
        data-testid="media-failure"
      >
        {{ session.failure?.message }}
      </AppAlert>

      <div class="flex flex-wrap items-center gap-3">
        <AppButton
          v-if="session.canRetry"
          :loading="session.busy"
          :disabled="session.busy"
          data-testid="retry-media"
          @click="retryMedia"
        >
          重试进入课堂
        </AppButton>

        <AppButton
          variant="secondary"
          :disabled="session.isClosedByTeacher"
          data-testid="leave-classroom"
          @click="leaveClassroom"
        >
          离开课堂
        </AppButton>

        <RouterLink
          v-if="session.isClosedByTeacher"
          :to="{ name: 'student-classrooms' }"
          class="text-sm text-ink-muted underline-offset-4 transition-colors hover:text-ink hover:underline"
          data-testid="back-to-classrooms"
        >
          返回我的课堂
        </RouterLink>
      </div>
    </template>
  </div>
</template>
