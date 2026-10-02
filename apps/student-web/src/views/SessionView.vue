<script setup lang="ts">
import { AppAlert, AppButton, AppCard, AppEmptyState, StatusDot } from '@classwatch/ui'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { describeConnectionQuality } from '../lib/connection-quality'
import { describeSessionPhase } from '../lib/media-session-state.ts'
import { describeScreenGateState } from '../lib/screen-capture-messages'
import { useMediaSessionStore } from '../stores/media-session.ts'
import { useScreenShareStore } from '../stores/screen-share'

/**
 * 课堂会话页（§55 / §56 / §22 / §49）—— Phase 6 的真实实现。
 *
 * 这一页只做四件事，别的一概不做：
 *
 * 1. **接收** PreJoin 交接过来的凭据与那条已经授权的屏幕轨道（§18 的顺序：
 *    先 Gate，后 join），连上 LiveKit 并把它发布成 ScreenShare（§20）。
 * 2. **只显示状态**（§56）：正在共享整屏 / 摄像头与麦克风未启用 / 网络质量 /
 *    当前状态。**没有**自己的屏幕预览——那会形成 screen inside screen inside
 *    screen，而且对"我到底有没有被看见"这个问题毫无帮助（页面上那句
 *    「正在共享整个屏幕」才是答案）。
 * 3. **在共享中断时给出一条恢复路径**（§22）：提示 + 「重新共享整个屏幕」，
 *    重新走一遍**完整**的 Gate（含 displaySurface 检查），然后发布新轨道。
 *    这里**不**重新 join——会话还在，只是轨道没了；重连是媒体链路的另一条路径。
 * 4. **在课堂被老师关闭时收尾**（§49）：断开媒体、停止捕获、回课堂列表。
 *    Phase 6 还没有 WebSocket，只能用低频轮询发现这件事（见 store 的说明）。
 *
 * 刷新页面的行为是刻意设计的：token 只存在内存里，刷新即丢失。页面因此显示
 * 「会话信息已丢失」并给回列表的入口——而不是拿一个过期的 token 去反复重连，
 * 那只会让学生盯着一个永远连不上的"连接中…"。
 */
const route = useRoute()
const router = useRouter()
const session = useMediaSessionStore()
const screenShare = useScreenShareStore()

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
          <p
            v-if="session.isOnline"
            class="text-base font-medium"
            data-testid="screen-sharing-status"
          >
            🖥 正在共享整个屏幕
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
              摄像头与麦克风是 Phase 9 / Phase 10 的内容（§24/§25）。
              这里刻意显示成**不可点的说明文字**而不是开关：一个点不动的假开关
              会让学生以为"打开失败"，而老师那端根本没有这个功能。
            -->
            <div class="space-y-1" data-testid="camera-row">
              <dt class="text-xs tracking-wide text-ink-muted uppercase">摄像头</dt>
              <dd class="text-ink">未启用（Phase 9 接入）</dd>
            </div>
            <div class="space-y-1" data-testid="microphone-row">
              <dt class="text-xs tracking-wide text-ink-muted uppercase">麦克风</dt>
              <dd class="text-ink">未启用（Phase 10 接入）</dd>
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
