<script setup lang="ts">
import { ApiError, isApiError } from '@classwatch/api-client'
import {
  AppAlert,
  AppButton,
  AppCard,
  AppEmptyState,
  ProtectedRouteGate,
  StatusDot,
} from '@classwatch/ui'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { classroomStatusLabel, classroomStatusTone } from '../lib/classroom-status'
import {
  checkScreenCaptureSupport,
  describeScreenCapture,
  toJoinCaptureDiagnostics,
  type ScreenCaptureSupport,
} from '../lib/screen-capture'
import { describeScreenGateFailure, describeScreenGateState } from '../lib/screen-capture-messages'
import { describeStudentClassroomError, isNotAssigned } from '../lib/student-classroom-error'
import { joinClassroom } from '../lib/student-sessions-api.ts'
import { useClassroomsStore } from '../stores/classrooms'
import { useMediaSessionStore } from '../stores/media-session.ts'
import { useScreenShareStore } from '../stores/screen-share'

/**
 * PreJoin —— 进入课堂前的最后一步（§15 Step 2；§16 / §17 / §18 / §43 / §57）。
 *
 * 这一页是学生**唯一**一次在"真的被看见"之前做决定的地方，因此它的结构是固定的：
 *
 * 1. 说明必须共享什么（§15 Step 2 的卡片）；
 * 2. §57 的隐私告知原文，且必须出现在**任何**捕获请求之前；
 * 3. 能力自检（§17）——不满足就禁用主按钮并把原因写清楚，**绝不**降级成窗口共享；
 * 4. 点击之后才请求整屏，并过 §16 的 `displaySurface` 硬 Gate；
 * 5. Gate 通过之后才调 join（§18：顺序不能变），把 Gate 的诊断值一并提交（§43），
 *    拿到 `{sessionId, livekitUrl, token}`、把凭据与**同一条**轨道交给会话 store，
 *    然后跳转到 `/student/session/:sessionId`。
 *
 * 第 5 步的关键约束：换页**不会**重新请求屏幕权限。轨道对象被整体移交（§20），
 * 会话页只是把它 publish 出去——学生全程只会看到一次授权弹窗。
 */
const route = useRoute()
const router = useRouter()
const store = useClassroomsStore()
const screenShare = useScreenShareStore()
const mediaSession = useMediaSessionStore()

/**
 * 能力自检（§17）。
 *
 * WHY 在 setup 里同步做、而不是等到点击时：学生需要在**点按钮之前**知道
 * "这个浏览器不行"。等到点击才拒绝，学生会先经历一次真实的授权弹窗与一次失败，
 * 然后才被告知要换浏览器——而他已经在课上了。
 *
 * 结果是常量（浏览器能力在一页的生命周期内不会变），因此用 `const` 而不是 ref。
 */
const support: ScreenCaptureSupport = checkScreenCaptureSupport()

/** 能力不足时的说明（§17）。原文与"Gate 读不到 displaySurface"共用同一句。 */
const unsupportedNotice = computed(() =>
  support.supported
    ? null
    : describeScreenGateFailure({
        kind: 'unsupported',
        reason: support.reason ?? 'no-getDisplayMedia',
      }),
)

/** 当前课堂 id；用它驱动加载，也用于"切换课堂时不残留上一个课堂"的判断。 */
const classroomId = computed(() => String(route.params.id ?? ''))

/**
 * `:id` 变化时重新加载，并在加载前先清空。
 *
 * WHY 先清空再请求：如果只发起新请求，从 A 切到 B 的那一瞬间页面上仍是 A 的
 * 标题与说明——学生会在"以为是 B"的页面上继续往下读（甚至去点共享屏幕）。
 * 清空后页面进入加载态，任何时刻显示的都只可能是"当前这个 id 的课堂"。
 *
 * 同时 `screenShare.reset()`：一条屏幕轨道只属于"当时那个课堂"，切课堂时不释放，
 * 学生会以为自己在给新课堂共享，实际那条轨道是上一个课堂的遗留物。
 */
watch(
  classroomId,
  (id) => {
    store.clearDetail()
    screenShare.reset()
    if (id) void store.fetchDetail(id)
  },
  { immediate: true },
)

/**
 * 离开页面时清空并**释放屏幕轨道**。
 *
 * WHY 必须释放：路由离开不会自动停止 MediaStreamTrack。不释放的后果不是
 * "多占一点内存"，而是浏览器顶部一直挂着"正在共享你的屏幕"——学生会以为自己
 * 还在被监督（或者更糟：在他以为已经离开课堂之后继续被采集）。
 * 这一处也顺带覆盖 §65 Case 14 的"退出课堂即停止捕获"。
 */
onBeforeUnmount(() => {
  store.clearDetail()
  screenShare.reset()
})

/** 未被授权 / 课堂不存在：两者共用 404 STUDENT_NOT_ASSIGNED，界面也不区分（§63）。 */
const notAssigned = computed(() => isNotAssigned(store.currentError))

/** 老师还没开启这堂课（§7 / docs/frontend/student.md §3.4）。 */
const isClosed = computed(() => store.current?.status === 'CLOSED')

/** 加载失败且不是"不在名单里"时，才给出重试入口。 */
const loadFailed = computed(
  () => store.currentError !== null && !store.current && !notAssigned.value,
)

/**
 * 主按钮是否可点：本页有课堂 + 课堂 OPEN + 浏览器能力足够，三者缺一不可。
 * join 请求飞行中另外禁用（store 内部还有一道重入保护，这里是给学生看的反馈）。
 */
const canStartShare = computed(
  () => store.current !== null && !isClosed.value && support.supported && !joining.value,
)

/**
 * 从 lost / error 恢复时文案不同：§22 的原文要求是「重新共享整个屏幕」。
 * 已经共享、只是 join 失败时改成"重试进入课堂"——那种情况下按钮**不会**再请求
 * 屏幕权限，措辞必须让学生知道这一点，否则他会以为要重新选一次共享范围。
 */
const startButtonLabel = computed(() => {
  if (screenShare.isSharing && joinError.value !== null) return '重试进入课堂'
  if (screenShare.isLost || screenShare.hasFailed) return '重新共享整个屏幕'
  return '共享整个屏幕并进入课堂'
})

/**
 * 按钮被禁用时的原因。必须有：一个灰掉却不说话的按钮，学生只会一直点它。
 * 只在按钮确实渲染出来、且确实不可点时才有值。
 */
const startButtonHint = computed(() => {
  if (!store.current || canStartShare.value || joining.value) return null
  if (isClosed.value) return '老师尚未开启本课堂。开启后重新打开这个页面即可共享整个屏幕并进入。'
  return '当前浏览器无法确认你是否共享了完整显示器，因此不能进入课堂。'
})

/**
 * Gate 拒绝 / 共享断开的说明。
 *
 * WHY 在这里把三种来源（能力自检 / Gate 拒绝 / track ended）折叠成同一段文案：
 * 它们对学生的含义是同一件事——"现在没有满足课堂要求的共享"。但折叠**只发生在这里**，
 * store 里三者仍是分开的字段，因为它们的恢复动作并不相同（一个要换浏览器，
 * 一个要重选共享面，一个只要重新共享）。
 */
const gateMessage = computed(() =>
  describeScreenGateState({
    isLost: screenShare.isLost,
    unsupportedReason: screenShare.unsupportedReason,
    failure: screenShare.failure,
  }),
)

/**
 * join 失败（§43 的 404 / 409 / 401 / 403）。
 *
 * 只保留 ApiError（带契约里的 code），非 ApiError 一律走通用文案：
 * 后端原始报文绝不进界面（§58）。
 */
const joinError = ref<ApiError | null>(null)
const joinErrorMessage = computed(() =>
  joinError.value === null ? null : describeStudentClassroomError(joinError.value),
)

/** join 请求飞行中（按钮 loading + 禁用，避免连点产生第二个 Session）。 */
const joining = ref(false)

/**
 * 这次失败是不是"再点一次也没用"。
 *
 * 不在名单里、课堂已关闭、账号被停用、登录失效——这四种情况下继续握着屏幕轨道
 * 没有任何意义，学生会带着"我还在共享"的错觉停在这一页。其余（网络、5xx、限流）
 * 属于可重试，轨道必须留着：Gate 已经通过，重试不该再弹一次授权窗口。
 */
function isFatalJoinError(error: ApiError): boolean {
  return (
    error.code === 'STUDENT_NOT_ASSIGNED' ||
    error.code === 'CLASSROOM_CLOSED' ||
    error.code === 'ACCOUNT_DISABLED' ||
    error.code === 'AUTH_REQUIRED'
  )
}

/**
 * 进入课堂：先 Gate，后 join（§18 的顺序不能变）。
 *
 * 已经共享过（join 失败后重试）时跳过 Gate，直接重发 join——这条路径下屏幕权限
 * 还在，再请求一次只会让浏览器弹第二次授权框（§20 明确禁止）。
 */
async function enterClassroom(): Promise<void> {
  if (joining.value) return

  if (!screenShare.isSharing) {
    // 必须在 click 处理函数里同步发起：getDisplayMedia 要求用户手势
    // （transient activation）。这里刻意不 await 别的事情。
    await screenShare.start()
    if (!screenShare.isSharing) return
  }

  const capture = screenShare.capture
  if (capture === null) return

  joining.value = true
  joinError.value = null
  try {
    const response = await joinClassroom(
      classroomId.value,
      toJoinCaptureDiagnostics(describeScreenCapture(capture)),
    )
    /**
     * 移交所有权：轨道**不停**，会话页要用同一条（§20）。
     * 顺序很关键——先 join 成功再移交，这样失败时轨道仍归本页管，
     * 学生可以直接重试进入课堂而不必重新选一次共享范围。
     */
    const handed = screenShare.handOff()
    if (handed === null) return
    mediaSession.prepare({
      sessionId: response.sessionId,
      classroomId: classroomId.value,
      credentials: { livekitUrl: response.livekitUrl, token: response.token },
      capture: handed,
    })
    await router.push({
      name: 'student-session',
      params: { sessionId: response.sessionId },
    })
  } catch (cause) {
    const error = isApiError(cause) ? cause : null
    joinError.value = error
    if (error !== null && isFatalJoinError(error)) {
      // 走不到课堂了：停掉共享，别让学生继续被看着（§22 的同一条理由）。
      screenShare.stop()
    }
  } finally {
    joining.value = false
  }
}

function stopScreenShare(): void {
  screenShare.stop()
  joinError.value = null
}

/**
 * 共享中状态里的诊断值。
 *
 * WHY 把 `displaySurface` 的原始值显示给**学生**：它是 §16 Gate 的唯一判据。
 * 出问题时（例如某个浏览器返回 'unknown'）学生截图这一行，老师立刻就知道 Gate
 * 读到了什么，而不必让学生口头描述他刚才点了哪个选项。
 * 只显示枚举值本身——设备名、窗口标题这类信息一个都不显示。
 */
const surfaceDetail = computed(() => screenShare.diagnostics?.rawDisplaySurface ?? '未提供')

function retry(): void {
  if (classroomId.value) void store.fetchDetail(classroomId.value)
}
</script>

<template>
  <div class="mx-auto flex max-w-3xl flex-col gap-6">
    <!-- 首次加载：还没有任何数据，绝不先渲染一个空课堂。 -->
    <ProtectedRouteGate
      v-if="store.currentLoading && !store.current"
      label="正在加载课堂…"
      hint="如果长时间没有反应，请检查网络连接。"
    />

    <!--
      404 STUDENT_NOT_ASSIGNED：不能停在空白页，也不能给"重试"（点一百次结果都一样）。
      唯一的下一步是联系老师，或者回列表看看别的课堂。
    -->
    <AppCard v-else-if="notAssigned">
      <AppEmptyState
        data-testid="classroom-not-assigned"
        title="你不在这个课堂的名单里"
        description="请联系老师确认是否已经把你加入这个课堂。确认后重新打开这个页面即可。"
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

    <!-- 其它失败（网络、5xx）：给重试，也给回列表的出口，不把学生困在这一页。 -->
    <AppCard v-else-if="loadFailed">
      <AppEmptyState
        tone="danger"
        data-testid="classroom-detail-error"
        title="课堂信息加载失败"
        :description="describeStudentClassroomError(store.currentError)"
      >
        <div class="flex items-center gap-2">
          <AppButton variant="secondary" size="sm" @click="retry">重试</AppButton>
          <RouterLink
            :to="{ name: 'student-classrooms' }"
            class="inline-flex h-8 items-center justify-center rounded-control px-3 text-xs font-medium text-ink-muted transition-colors hover:bg-surface-muted hover:text-ink"
            data-testid="back-to-classrooms"
          >
            返回我的课堂
          </RouterLink>
        </div>
      </AppEmptyState>
    </AppCard>

    <template v-else-if="store.current">
      <header class="space-y-2">
        <h1 class="text-2xl font-semibold tracking-tight" data-testid="classroom-name">
          {{ store.current.name }}
        </h1>
        <p
          v-if="store.current.description"
          class="max-w-2xl text-sm leading-relaxed text-ink-muted"
          data-testid="classroom-description"
        >
          {{ store.current.description }}
        </p>
        <div class="flex flex-wrap items-center gap-x-4 gap-y-1">
          <span class="text-sm text-ink-muted" data-testid="classroom-teacher">
            {{ store.current.teacher.displayName }}
          </span>
          <StatusDot
            :status="classroomStatusTone(store.current.status)"
            :label="classroomStatusLabel(store.current.status)"
            data-testid="classroom-status"
          />
        </div>
      </header>

      <!-- CLOSED：说清"是老师还没开"，而不是"你不能进"。等老师开启后本页会正常放行。 -->
      <AppAlert v-if="isClosed" tone="info" data-testid="classroom-closed-notice">
        老师尚未开启本课堂。现在还不能进入，等老师开启后重新打开这个页面即可。
      </AppAlert>

      <AppCard>
        <div class="space-y-2">
          <h2 class="text-lg font-semibold tracking-tight">本课堂需要共享整个电脑屏幕</h2>
          <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">
            进入课堂后，老师需要看到整块显示器上的内容。只共享某个窗口或浏览器标签页无法进入课堂。
          </p>
        </div>

        <div class="mt-6 grid gap-6 border-t border-border-subtle pt-5 sm:grid-cols-2">
          <section class="space-y-2" data-testid="prejoin-required">
            <h3 class="text-xs font-medium tracking-wide text-ink-muted uppercase">必须</h3>
            <ul class="space-y-2">
              <li class="flex gap-2 text-sm">
                <span aria-hidden="true">🖥</span>
                <span class="leading-relaxed">
                  整个显示器
                  <span class="block text-ink-muted">
                    老师需要看到整块屏幕上的内容，包括你在其他应用里打开的内容。
                  </span>
                </span>
              </li>
            </ul>
          </section>

          <section class="space-y-2" data-testid="prejoin-optional">
            <h3 class="text-xs font-medium tracking-wide text-ink-muted uppercase">可选</h3>
            <ul class="space-y-1 text-sm leading-relaxed">
              <li class="flex gap-2">
                <span aria-hidden="true">📷</span>
                <span>摄像头</span>
              </li>
              <li class="flex gap-2">
                <span aria-hidden="true">🎤</span>
                <span>麦克风</span>
              </li>
            </ul>
            <p class="text-xs leading-relaxed text-ink-muted">可选，不影响进入课堂。</p>
          </section>
        </div>
      </AppCard>

      <!--
        §57 隐私告知 —— **原文，不得改写、不得省略**。
        它必须先于任何屏幕捕获请求出现：这是学生唯一一次在"真的被看见"之前
        知道这件事的机会。Phase 5 起这一页真的会请求共享，因此它必须留在
        能力自检与主按钮**上方**（DOM 顺序 = 阅读顺序 = 学生看到的顺序）。
      -->
      <AppAlert tone="info" data-testid="privacy-notice">
        进入课堂后，老师能够看到当前共享显示器上的内容，包括你在其他应用程序和浏览器中打开的内容。请关闭与课堂无关的隐私信息后再继续。
      </AppAlert>

      <!--
        §17 能力自检失败：**阻断式**说明。主按钮保持渲染但禁用（而不是藏起来）——
        按钮消失会让学生以为是自己操作错了；灰掉的按钮 + 一句原因才是可以自己解决的死路。
      -->
      <AppAlert
        v-if="unsupportedNotice"
        tone="danger"
        :title="unsupportedNotice.title"
        data-testid="screen-unsupported-notice"
      >
        {{ unsupportedNotice.description }}
      </AppAlert>

      <!-- 共享就绪：§56 要求只给状态指示，**不**显示自己的画面预览（避免 screen in screen）。 -->
      <AppCard v-if="screenShare.isSharing" data-testid="screen-sharing">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div class="space-y-1">
            <p class="text-base font-medium" data-testid="screen-sharing-status">
              🖥 正在共享整个屏幕
            </p>
            <p class="text-xs text-ink-muted">
              已确认的共享范围：<span data-testid="screen-surface">{{ surfaceDetail }}</span>
            </p>
          </div>
          <AppButton variant="secondary" data-testid="stop-screen-share" @click="stopScreenShare">
            停止共享
          </AppButton>
        </div>

        <!--
          join 正在进行：说明"共享已就绪，正在进入课堂"。
          这句话必须存在——否则学生会以为"正在共享"就等于"已经在课堂里了"，
          然后坐在这页上等老师看到自己。
        -->
        <p v-if="joining" class="mt-4 text-sm text-ink-muted" data-testid="joining-classroom">
          整屏共享已就绪，正在进入课堂…
        </p>
      </AppCard>

      <!--
        join 失败（§43）：404 不在名单 / 409 课堂已关闭 / 网络错误。
        可重试的失败**保留**已经通过的共享——再点一次按钮只会重发 join，
        不会重新弹一次屏幕授权（§20）。
      -->
      <AppAlert v-if="joinErrorMessage" tone="danger" title="进入课堂失败" data-testid="join-error">
        {{ joinErrorMessage }}
      </AppAlert>

      <!--
        §22：共享被结束（学生点了浏览器的"停止共享"，或浏览器自己结束了轨道）。
        这不是"错误"而是"当前状态不满足课堂要求"，所以用 ⚠ + 一个明确的恢复动作，
        并且**留在本页**——跳走或伪装成功都会在老师那端留下一个没有屏幕的"在线"学生。
      -->
      <AppCard v-if="screenShare.isLost" data-testid="screen-lost">
        <div class="space-y-2">
          <h2 class="text-base font-medium">⚠ 已停止屏幕共享</h2>
          <p class="text-sm leading-relaxed text-ink-muted">当前课堂要求持续共享整个屏幕。</p>
        </div>
      </AppCard>

      <!--
        Gate 拒绝：就在原地把原因说清楚（选了窗口 / 选了标签页 / 系统没给屏幕录制权限……）。
        每句文案都必须带"下一步做什么"，因为学生此刻唯一的出路就是再点一次按钮。
      -->
      <AppAlert
        v-if="gateMessage"
        tone="danger"
        :title="gateMessage.title"
        data-testid="screen-gate-error"
      >
        {{ gateMessage.description }}
      </AppAlert>

      <div class="space-y-3">
        <AppButton
          block
          :disabled="!canStartShare || screenShare.isRequesting"
          :loading="screenShare.isRequesting || joining"
          data-testid="enter-classroom"
          @click="enterClassroom"
        >
          {{ startButtonLabel }}
        </AppButton>

        <p
          v-if="startButtonHint"
          class="text-xs leading-relaxed text-ink-muted"
          data-testid="enter-disabled-hint"
        >
          {{ startButtonHint }}
        </p>

        <p
          v-if="screenShare.isRequesting"
          class="text-xs leading-relaxed text-ink-muted"
          data-testid="screen-requesting"
        >
          请在浏览器的选择窗口里选择「整个屏幕」，然后点击共享。
        </p>
      </div>

      <div>
        <RouterLink
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
