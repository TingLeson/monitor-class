<script setup lang="ts">
import type { MonitorStudent } from '@classwatch/shared-types'
import {
  AppAlert,
  AppBadge,
  AppButton,
  AppCard,
  AppEmptyState,
  ProtectedRouteGate,
} from '@classwatch/ui'
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import MonitorFocusPanel from '../components/MonitorFocusPanel.vue'
import MonitorTile from '../components/MonitorTile.vue'
import { describeClassroomError } from '../lib/classroom-error.ts'
import { describeTalkText } from '../lib/private-talk.ts'
import { describeRealtimeStatus } from '../lib/realtime-status.ts'
import { useClassroomsStore } from '../stores/classrooms.ts'
import { useMonitorStore } from '../stores/monitor.ts'
import { useRealtimeStore } from '../stores/realtime.ts'

/**
 * 课堂监督墙（§29 / §30 / §51 / §52 / §73）—— Phase 7。
 *
 * 这一页要同时成立三件事：
 * 1. **看到**：每个被授权的学生一张卡片，主体是桌面屏幕（§29）；
 * 2. **看得动**：点开某个学生进入 Focus 大画面（§30），Esc 回来；
 * 3. **看得起**：只订阅视口里的画面、网格低画质、Focus 高画质、切走标签页就停（§52）。
 *
 * 数据来自三路，合成卡片时界线必须清楚（§51）：
 * - `GET /teacher/classrooms/:id/monitor`：**首屏快照 + 兜底**（60 秒 / 断线时 20 秒）
 *   —— 谁在上课、屏幕是否中断，以及**名单的权威**；
 * - `/ws/teacher` 的实时事件（§47）：上线/下线/屏幕中断/恢复的**增量**，
 *   让徽章与计数在事件发生的那一刻就变，而不是等下一次快照；
 * - LiveKit 手动订阅：画面有没有到 —— 媒体状态。
 *
 * 刻意**不做**的：摄像头画中画（§29 的 CAM 角标）与 Focus 里的摄像头预览在 Phase 9 完成；
 * 私密语音（§31）与"听学生麦克风"（§32）在 Phase 10 完成——后者只订**Focus 那一个**，
 * 因为多路音频混在一起之后老师分辨不出是谁在说话。
 */
const route = useRoute()
const classrooms = useClassroomsStore()
const monitor = useMonitorStore()
const realtime = useRealtimeStore()

/**
 * 实时状态提示（§47）：只在**没连上**时出现。
 *
 * 为什么必须有：监督墙的整个价值是"老师看到的就是现在发生的"。通道断了而界面
 * 什么都不说，老师会继续把陈旧的画面与徽章当成现状来做判断（§51 的整个 DTO
 * 设计就是为了防这件事）。措辞保持中性并给出动作（重新加载）。
 */
const realtimeDisplay = computed(() =>
  describeRealtimeStatus(realtime.state, {
    authFailed: realtime.authFailed,
    wasConnected: realtime.wasConnected,
  }),
)

const classroomId = computed(() => String(route.params.id ?? ''))

/** 课堂标题栏的数据来自详情接口（monitor 响应里没有课堂名）。 */
const classroom = computed(() => classrooms.current)

/** 与列表页 / 详情页同一句话：V1 只有 OPEN / CLOSED 两个状态（§7）。 */
function statusLabel(status: 'OPEN' | 'CLOSED'): string {
  return status === 'OPEN' ? '已开启' : '未开启'
}

/**
 * Focus 是**页面内状态**，不是路由（见 MonitorFocusPanel 顶部那段决策说明）。
 *
 * 这里只多存一个"是哪张卡片打开的"元素引用：关闭 Focus 时把键盘焦点还回去，
 * 否则键盘用户每关一次都要从页面开头重新 Tab 一遍。
 */
let focusTrigger: HTMLElement | null = null

function openFocus(student: MonitorStudent, event: Event): void {
  focusTrigger = event.currentTarget instanceof HTMLElement ? event.currentTarget : null
  monitor.setFocusedStudent(student.studentId)
}

function closeFocus(): void {
  monitor.setFocusedStudent(null)
  focusTrigger?.focus()
  focusTrigger = null
}

/* -------------------------------------------------------------------------- */
/* 可见性与页面可见性（§52 的两路输入）                                        */
/* -------------------------------------------------------------------------- */

/**
 * 卡片进/出视口 → 交给 store 重算订阅计划。
 *
 * WHY 由视图转发而不是卡片直接改 store：卡片是展示组件（它连 store 都不认识），
 * "可见 = 要订阅"是媒体策略，只应该存在于 store 里。
 */
function onTileVisibility(studentId: string, visible: boolean): void {
  monitor.setStudentVisible(studentId, visible)
}

/**
 * 页面隐藏时降载，回到页面再恢复（§52）。
 *
 * WHY 老师端可以这么做，学生端不行（§23）：学生端的任务是持续共享整块屏幕，
 * 老师随时可能在看，所以学生端**不能**因为自己页面不可见就改变行为——
 * Chrome 最小化不是异常。老师端是纯接收方，页面不可见时那些像素没有任何人看，
 * 继续下行只是白烧带宽。
 */
function onVisibilityChange(): void {
  monitor.setPageHidden(document.visibilityState === 'hidden')
}

onMounted(() => {
  const id = classroomId.value
  if (!id) return
  /**
   * 两件事并行，互不阻塞：
   * - 详情只为页头（名称 / 状态 / 人数）；
   * - monitor + 媒体连接才是这一页的主体，失败也有各自的重试入口。
   */
  void classrooms.fetchDetail(id)
  void monitor.load(id)
  document.addEventListener('visibilitychange', onVisibilityChange)
  /**
   * 先对一次时：老师用"在新标签页打开链接"的方式进来时，页面从一开始就是隐藏的——
   * 只等事件的话，第一帧就会订阅一整屏根本没人看的画面。
   */
  onVisibilityChange()
})

/**
 * 离开页面时断开媒体并停止轮询。
 *
 * 不 await：卸载路径上没人能等；`stop()` 内部的断开是 fire-and-forget，
 * 而本地状态（订阅表、定时器）是同步清掉的。老师关掉监督墙后，
 * LiveKit 那边也会在连接关闭后把订阅一起撤掉。
 */
onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisibilityChange)
  classrooms.clearDetail()
  void monitor.stop()
})

/** monitor 接口失败（业务面）：保留已有画面，只提示"数据没刷新"。 */
const monitorErrorMessage = computed(() =>
  monitor.error === null ? null : describeClassroomError(monitor.error),
)

const loadingFirstTime = computed(() => !monitor.loaded && monitor.students.length === 0)

/** 头部计数（§29 的 "18 / 25"）：M = 名单总数，N = 已进入。 */
const enteredLabel = computed(() => `已进入 ${monitor.enteredCount} / 共 ${monitor.rosterCount}`)

/** 名单里有学生，但一个都还没进来时给一句解释——一行行灰卡片会让人以为页面坏了。 */
const nobodyEntered = computed(() => monitor.rosterCount > 0 && monitor.enteredCount === 0)

/* -------------------------------------------------------------------------- */
/* 私密语音与老师麦克风（§27 / §31 / §32）                                     */
/* -------------------------------------------------------------------------- */

/**
 * 失败提示只属于**点它的那个学生**。
 *
 * 失败信息存在 store 里是全局的，而面板是按学生切换的：不过滤的话，老师对张三发起
 * 失败之后点开李四，会在李四的面板上看到一句关于张三的错误。
 * `setFocusedStudent` 也会清掉它（见 store），这里再挡一道。
 */
const focusedTalkError = computed(() => {
  const focused = monitor.focusedStudent
  if (focused === null || monitor.talkTarget !== null) return null
  return monitor.talkFailure?.message ?? null
})

/** 失败原因是不是"老师还没开麦"（决定要不要给一键开麦入口，§31）。 */
const focusedTalkMicRequired = computed(
  () => monitor.focusedStudent !== null && monitor.talkFailure?.micRequired === true,
)

/**
 * 页头那句"正在与 X 语音沟通"（§31）。
 *
 * WHY 放在页头而不是只放在 Focus 面板里：老师退出 Focus 回到网格之后，
 * 面板就没了——而麦克风还在对那一个人广播。音频是看不见的，
 * 界面上必须有一处**始终**在回答"我现在在对谁讲话"。
 */
const talkTargetText = computed(() =>
  monitor.talkTarget === null ? null : describeTalkText(monitor.talkTarget.displayName),
)

/** Focus 面板上的「语音沟通」：目标就是当前 Focus 的学生（不可能是别人）。 */
function startTalkWithFocused(): void {
  const focused = monitor.focusedStudent
  if (focused === null) return
  void monitor.startTalk(focused)
}
</script>

<template>
  <div class="mx-auto flex max-w-7xl flex-col gap-6">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <RouterLink
        v-if="classroomId"
        :to="{ name: 'teacher-classroom-detail', params: { id: classroomId } }"
        class="text-sm text-ink-muted underline-offset-4 transition-colors hover:text-ink hover:underline"
        data-testid="monitor-back"
      >
        ← 返回课堂详情
      </RouterLink>
      <span class="text-xs text-ink-muted" data-testid="monitor-subscribed-count">
        正在监督 {{ monitor.subscribedCount }} 路画面
      </span>
    </div>

    <header class="space-y-2">
      <h1 class="text-2xl font-semibold tracking-tight" data-testid="monitor-title">课堂监督墙</h1>
      <div v-if="classroom" class="flex flex-wrap items-center gap-x-4 gap-y-1">
        <span class="text-sm text-ink" data-testid="monitor-classroom-name">
          {{ classroom.name }}
        </span>
        <AppBadge
          :label="statusLabel(classroom.status)"
          :tone="classroom.status === 'OPEN' ? 'positive' : 'muted'"
          data-testid="monitor-classroom-status"
        />
        <!--
          §29 的头部计数。M 来自后端返回的名单长度（Phase 7 起是**全部**被授权学生），
          N 用与徽章完全相同的判据（在线且有会话），两个数字必须能互相对上。
        -->
        <span class="text-sm text-ink-muted" data-testid="monitor-entered-count">
          {{ enteredLabel }}
        </span>
        <!--
          §31：当前私密语音目标。它是**始终可见**的那一处界面记忆——
          Focus 面板会随着切换学生消失，而麦克风还在对那一个人广播。
        -->
        <span v-if="talkTargetText" class="text-sm text-ink" data-testid="monitor-talk-target">
          🎤 {{ talkTargetText }}
        </span>
      </div>
    </header>

    <!--
      §27/§31：老师自己的麦克风开关。老师 Token 唯一允许发布的源就是 microphone，
      而私密语音必须先有它——没开麦就点「语音沟通」会被后端以 TEACHER_MIC_REQUIRED
      拒绝，所以这个入口必须显眼且随时可用（也在 Focus 面板里给了一键开麦）。
    -->
    <AppCard data-testid="teacher-mic-panel">
      <div class="flex flex-wrap items-center gap-3">
        <span class="text-sm font-medium text-ink">我的麦克风</span>
        <AppButton
          size="sm"
          variant="secondary"
          data-testid="toggle-teacher-mic"
          :loading="monitor.isTeacherMicRequesting"
          :disabled="monitor.isTeacherMicRequesting"
          @click="monitor.toggleTeacherMicrophone()"
        >
          {{ monitor.teacherMicActionLabel }}
        </AppButton>
        <span class="text-sm text-ink-muted" data-testid="teacher-mic-status">
          {{ monitor.isTeacherMicOn ? '已开启' : '未开启' }}
        </span>
        <span
          v-if="monitor.teacherMicFailure"
          class="text-xs text-status-danger"
          data-testid="teacher-mic-failure"
        >
          {{ monitor.teacherMicFailure }}
        </span>
      </div>
      <p class="mt-2 text-xs leading-relaxed text-ink-muted">
        私密语音只会让你选中的那一个学生听到；其他学生收不到任何提示。
      </p>
    </AppCard>

    <!--
      媒体面失败：给明确提示 + 重试入口，绝不白屏。
      业务状态（谁在上课）与媒体状态是两件事，所以这里的提示**不**影响下面的卡片列表。
    -->
    <!--
      课堂结束（§49）：说清楚"画面没了是因为课结束了"，而不是让老师对着一屏
      灰卡片猜是不是系统坏了。媒体是**主动**释放的（见 store），所以这里
      也不会同时出现下面那条媒体失败告警。
    -->
    <AppAlert
      v-if="monitor.classroomClosed"
      tone="info"
      title="本课堂已经结束"
      data-testid="monitor-classroom-closed"
    >
      老师已关闭本课堂，学生端的课堂同步结束，画面已经释放。重新开启课堂后，学生需要重新进入。
    </AppAlert>

    <AppAlert
      v-if="monitor.mediaError && !monitor.classroomClosed"
      tone="danger"
      title="无法看到学生画面"
      data-testid="monitor-media-error"
    >
      {{ monitor.mediaError }}
      <div class="mt-3">
        <AppButton size="sm" data-testid="monitor-media-retry" @click="monitor.retryMedia()">
          重试连接
        </AppButton>
      </div>
    </AppAlert>

    <!-- 实时通道没连上：状态可能不是最新。连上时这一条不出现。 -->
    <AppAlert
      v-if="realtime.started && realtime.state !== 'open'"
      :tone="realtimeDisplay.tone === 'warning' ? 'danger' : 'info'"
      :title="realtimeDisplay.label"
      data-testid="monitor-realtime-status"
      :data-realtime-state="realtime.state"
    >
      {{ realtimeDisplay.hint }}
      <div class="mt-3 flex flex-wrap gap-2">
        <AppButton size="sm" variant="secondary" @click="monitor.refresh()">重新加载</AppButton>
        <!-- 自动重连已停止时（连续多次连不上）才出现，见 realtime store 的 restart()。 -->
        <AppButton
          v-if="realtime.authFailed"
          size="sm"
          variant="secondary"
          data-testid="monitor-realtime-retry"
          @click="realtime.restart()"
        >
          重试实时连接
        </AppButton>
      </div>
    </AppAlert>

    <AppAlert
      v-if="monitorErrorMessage"
      tone="info"
      title="监督数据没有刷新"
      data-testid="monitor-data-error"
    >
      {{ monitorErrorMessage }}
      <div class="mt-3">
        <AppButton size="sm" variant="secondary" @click="monitor.refresh()">重新加载</AppButton>
      </div>
    </AppAlert>

    <ProtectedRouteGate
      v-if="loadingFirstTime"
      label="正在连接课堂…"
      hint="正在申请媒体凭据并加载学生状态。"
    />

    <AppCard v-else-if="monitor.isEmpty" data-testid="monitor-empty">
      <AppEmptyState
        title="这个课堂还没有学生"
        description="把学生加入课堂名单后，他们进入课堂时这里会出现对应的桌面画面。"
      />
    </AppCard>

    <!--
      监督墙网格（§29）。每个学生一张卡片——**包括没进入的**：
      老师打开这一页最想知道的就是"25 个人里进来了几个、谁还没进来"，
      只渲染有画面的那几张等于把这份信息藏起来。
      卡片顺序沿用后端返回的顺序（按 account 升序，排序是后端的权威，前端不重排）。
    -->
    <template v-else>
      <p v-if="nobodyEntered" class="text-sm text-ink-muted" data-testid="monitor-nobody-entered">
        名单上的学生都还没有进入本次课堂。他们进入后，这里会自动出现画面。
      </p>

      <div
        class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4"
        data-testid="monitor-grid"
      >
        <MonitorTile
          v-for="student in monitor.students"
          :key="student.studentId"
          :student="student"
          :subscription="monitor.subscriptionOf(student)"
          :media-state="monitor.mediaStateOf(student)"
          :camera-subscription="monitor.cameraSubscriptionOf(student)"
          :camera-media-state="monitor.cameraMediaStateOf(student)"
          :selected="monitor.focusedStudentId === student.studentId"
          :talk-target="monitor.isTalkTarget(student)"
          @open="openFocus(student, $event)"
          @visibility-change="onTileVisibility(student.studentId, $event)"
        />
      </div>
    </template>

    <!--
      Focus View（§30）。叠加在网格之上而不是替换它：网格仍然"在视口里"，
      IntersectionObserver 的可见性集合因此保持不变——退出 Focus 时不会有一批
      卡片因为重新挂载而重新订阅（§52 明确要求切换 Focus 不动其它可见卡片）。
    -->
    <MonitorFocusPanel
      v-if="monitor.focusedStudent"
      :student="monitor.focusedStudent"
      :subscription="monitor.subscriptionOf(monitor.focusedStudent)"
      :media-state="monitor.mediaStateOf(monitor.focusedStudent)"
      :camera-subscription="monitor.cameraSubscriptionOf(monitor.focusedStudent)"
      :camera-media-state="monitor.cameraMediaStateOf(monitor.focusedStudent)"
      :talk-target="monitor.isTalkTarget(monitor.focusedStudent)"
      :talk-busy="monitor.talkBusy"
      :talk-error="focusedTalkError"
      :talk-mic-required="focusedTalkMicRequired"
      :teacher-mic-state="monitor.teacherMicState"
      :microphone-subscription="monitor.microphoneSubscriptionOf(monitor.focusedStudent)"
      :microphone-media-state="monitor.microphoneMediaStateOf(monitor.focusedStudent)"
      @close="closeFocus"
      @start-talk="startTalkWithFocused"
      @stop-talk="monitor.stopTalk()"
      @enable-mic="monitor.enableTeacherMicrophone()"
    />
  </div>
</template>
