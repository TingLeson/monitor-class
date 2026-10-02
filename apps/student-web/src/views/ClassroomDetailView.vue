<script setup lang="ts">
import {
  AppAlert,
  AppButton,
  AppCard,
  AppEmptyState,
  ProtectedRouteGate,
  StatusDot,
} from '@classwatch/ui'
import { computed, onBeforeUnmount, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { classroomStatusLabel, classroomStatusTone } from '../lib/classroom-status'
import { describeStudentClassroomError, isNotAssigned } from '../lib/student-classroom-error'
import { useClassroomsStore } from '../stores/classrooms'

/**
 * PreJoin —— 进入课堂前的最后一步（§15 Step 2；docs/frontend/student.md §3）。
 *
 * 这一页**不做任何媒体操作**。它的全部价值是把"接下来会发生什么"提前讲清楚，
 * 让学生自己决定要不要继续：
 *
 * - 必须共享**整个显示器**（§16），不是窗口、不是标签页；
 * - 摄像头与麦克风是**可选**的（§24 / §25），不授权也能进课堂；
 * - §57 的隐私告知必须原文出现，且出现在任何屏幕捕获请求**之前**。
 *
 * Phase 4 里"进入课堂"就到此为止（§70）：主按钮是**禁用**的，并且明确写出
 * 整屏共享与进入课堂属于 Phase 5。宁可按钮点不动 + 说明原因，也不要做一个
 * "看起来能点、点了没反应"的按钮——后者会让学生以为系统坏了。
 */
const route = useRoute()
const store = useClassroomsStore()

/** 当前课堂 id；用它驱动加载，也用于"切换课堂时不残留上一个课堂"的判断。 */
const classroomId = computed(() => String(route.params.id ?? ''))

/**
 * `:id` 变化时重新加载，并在加载前先清空。
 *
 * WHY 先清空再请求：如果只发起新请求，从 A 切到 B 的那一瞬间页面上仍是 A 的
 * 标题与说明——学生会在"以为是 B"的页面上继续往下读（甚至去点共享屏幕）。
 * 清空后页面进入加载态，任何时刻显示的都只可能是"当前这个 id 的课堂"。
 */
watch(
  classroomId,
  (id) => {
    store.clearDetail()
    if (id) void store.fetchDetail(id)
  },
  { immediate: true },
)

/**
 * 离开页面时清空。
 *
 * WHY 不能只靠 watch：返回列表后 store 仍会留着最后一个课堂。它既没有任何用处，
 * 又会在下一次进入时短暂地作为"上一个课堂"被渲染出来（同样的问题，只是换个时机）。
 */
onBeforeUnmount(() => {
  store.clearDetail()
})

/** 未被授权 / 课堂不存在：两者共用 404 STUDENT_NOT_ASSIGNED，界面也不区分（§63）。 */
const notAssigned = computed(() => isNotAssigned(store.currentError))

/** 老师还没开启这堂课（§7 / docs/frontend/student.md §3.4）。 */
const isClosed = computed(() => store.current?.status === 'CLOSED')

/** 加载失败且不是"不在名单里"时，才给出重试入口。 */
const loadFailed = computed(
  () => store.currentError !== null && !store.current && !notAssigned.value,
)

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
        知道这件事的机会（Phase 5 才会真正请求共享，这行字此刻就要就位）。
      -->
      <AppAlert tone="info" data-testid="privacy-notice">
        进入课堂后，老师能够看到当前共享显示器上的内容，包括你在其他应用程序和浏览器中打开的内容。请关闭与课堂无关的隐私信息后再继续。
      </AppAlert>

      <div class="space-y-3">
        <AppButton block disabled data-testid="enter-classroom">共享整个屏幕并进入课堂</AppButton>
        <!--
          Phase 5 的说明用 title + 正文，而不是塞进一句话里：Prettier 只会在空格处
          折行，中文译文一旦被折行，"Phase 5 接入"这句原文就不再是一段完整的文字。
        -->
        <AppAlert
          tone="info"
          title="整屏共享与进入课堂将在 Phase 5 接入"
          data-testid="phase-notice"
        >
          届时这里会请求整块显示器的共享权限、校验共享范围，然后才连接课堂。
        </AppAlert>
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
