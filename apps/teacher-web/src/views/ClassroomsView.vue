<script setup lang="ts">
import { isApiError } from '@classwatch/api-client'
import type { Classroom } from '@classwatch/shared-types'
import {
  AppAlert,
  AppBadge,
  AppButton,
  AppCard,
  AppEmptyState,
  AppModal,
  ProtectedRouteGate,
} from '@classwatch/ui'
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import {
  classroomConflictNotice,
  describeClassroomError,
  isClassroomStateConflict,
} from '../lib/classroom-error'
import { formatTimeOfDay } from '../lib/format'
import { useClassroomsStore } from '../stores/classrooms'

/**
 * 我的课堂列表（§7 / §42 Teacher / §55；docs/frontend/teacher.md §2）。
 *
 * 三条界面契约：
 * 1. 列表只包含自己拥有的课堂——后端按 owner 过滤，前端不筛选也无从筛选（§14）；
 * 2. 开启 / 关闭是**状态迁移**（§48 / §49），必须二次确认并说明后果；
 * 3. 冲突（409）不是"操作失败"，而是"你看到的界面已经过期"：要刷新列表并讲清真实状态。
 */
const store = useClassroomsStore()

/** 当前等待确认的课堂；null 表示没有弹窗。 */
const toggleTarget = ref<Classroom | null>(null)
/** 弹窗内的失败提示（不需要刷新列表，用户可以直接在弹窗里重试）。 */
const dialogError = ref<string | null>(null)
/** 页面级提示：操作结果，以及"状态已被别处改变，列表已刷新"这类冲突说明。 */
const notice = ref<{ tone: 'success' | 'info'; text: string } | null>(null)

/**
 * 进入列表页就刷新一次。
 *
 * WHY 不只在 hasLoaded 为 false 时加载：课堂状态会被另一个标签页、另一台设备改变，
 * 而"我看到的到底是不是现在的状态"是这一页唯一重要的事。列表规模是个位数，
 * 刷新一次的代价远小于让老师对着过期状态点开关。
 */
void store.fetchList()

/** 开关课堂是否正在进行（只给确认按钮转圈，其余按钮只是禁用）。 */
const toggling = computed(() => store.pendingAction === 'open' || store.pendingAction === 'close')

function statusLabel(classroom: Classroom): string {
  return classroom.status === 'OPEN' ? '已开启' : '未开启'
}

function toggleLabel(classroom: Classroom): string {
  return classroom.status === 'OPEN' ? '关闭课堂' : '开启课堂'
}

function openToggleDialog(classroom: Classroom): void {
  // 按钮已经禁用，这里再挡一次：键盘或脚本触发的点击同样不该产生第二个请求。
  if (store.pendingId !== null) return
  dialogError.value = null
  notice.value = null
  toggleTarget.value = classroom
}

function closeToggleDialog(): void {
  toggleTarget.value = null
  dialogError.value = null
}

/**
 * 执行状态迁移。
 *
 * 成功路径用返回的 DTO 更新那一行（在 store 里做），因此"本次开始于"立刻反映
 * 这一次新建的 Run（§8），不需要重新拉列表。
 */
async function confirmToggle(): Promise<void> {
  const target = toggleTarget.value
  if (!target || store.pendingId !== null) return

  dialogError.value = null
  notice.value = null
  const opening = target.status === 'CLOSED'
  try {
    const response = opening ? await store.open(target.id) : await store.close(target.id)
    toggleTarget.value = null
    notice.value = {
      tone: 'success',
      text: opening
        ? `课堂「${response.classroom.name}」已开启，被授权的学生现在可以进入。`
        : `课堂「${response.classroom.name}」已关闭，本次课堂运行已结束。`,
    }
  } catch (cause) {
    const code = isApiError(cause) ? cause.code : null

    if (code !== null && isClassroomStateConflict(code)) {
      // 另一个标签页/设备已经改过状态。关掉弹窗、刷新列表、把真实状态说出来——
      // 只弹一句"操作失败"会让老师怀疑自己点错了按钮（teacher.md §5）。
      closeToggleDialog()
      notice.value = { tone: 'info', text: classroomConflictNotice(code) ?? '' }
      await store.fetchList()
      return
    }

    if (code === 'CLASSROOM_NOT_FOUND' || code === 'CLASSROOM_NOT_OWNER') {
      // 这一行已经失效（课堂被删，或根本不是自己的）：刷新后它就该消失 / 不该存在。
      closeToggleDialog()
      notice.value = { tone: 'info', text: describeClassroomError(cause) }
      await store.fetchList()
      return
    }

    dialogError.value = describeClassroomError(cause)
  }
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <header class="flex flex-wrap items-end justify-between gap-3">
      <div class="space-y-1">
        <h1 class="text-2xl font-semibold tracking-tight">我的课堂</h1>
        <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">
          这里只有你自己创建的课堂。开启课堂后，名单里的学生即可进入并共享整个屏幕。
        </p>
      </div>
      <!-- 用链接而不是 router.push 的按钮：课堂列表也该能被中键/新标签页打开。 -->
      <RouterLink
        :to="{ name: 'teacher-classroom-new' }"
        class="inline-flex h-10 shrink-0 items-center justify-center rounded-control bg-ink px-4 text-sm font-medium text-surface transition-colors hover:bg-ink/90"
      >
        新建课堂
      </RouterLink>
    </header>

    <AppAlert v-if="notice" :tone="notice.tone" data-testid="classrooms-notice">
      {{ notice.text }}
      <template #actions>
        <AppButton variant="ghost" size="sm" @click="notice = null">知道了</AppButton>
      </template>
    </AppAlert>

    <!-- 首次加载：还没有任何数据可显示，用占位而不是"还没有课堂"。 -->
    <ProtectedRouteGate
      v-if="store.loading && !store.hasLoaded"
      label="正在加载课堂列表…"
      hint="如果长时间没有反应，请检查网络连接。"
    />

    <!-- 加载失败与"没有数据"必须长得不一样，否则用户会把故障当成"课堂都没了"。 -->
    <AppCard v-else-if="store.error">
      <AppEmptyState
        tone="danger"
        data-testid="classrooms-error"
        title="课堂列表加载失败"
        :description="describeClassroomError(store.error)"
      >
        <AppButton variant="secondary" size="sm" @click="store.fetchList()">重试</AppButton>
      </AppEmptyState>
    </AppCard>

    <AppCard v-else-if="store.isEmpty">
      <AppEmptyState
        data-testid="classrooms-empty"
        title="还没有课堂"
        description="创建第一个课堂后，就能在这里开启它，并把学生加入名单。"
      >
        <RouterLink
          :to="{ name: 'teacher-classroom-new' }"
          class="inline-flex h-8 items-center justify-center rounded-control border border-border-subtle bg-surface px-3 text-xs font-medium text-ink transition-colors hover:bg-surface-muted"
        >
          新建课堂
        </RouterLink>
      </AppEmptyState>
    </AppCard>

    <template v-else>
      <p v-if="store.loading" class="text-xs text-ink-muted" role="status">正在更新列表…</p>

      <ul class="grid gap-4 md:grid-cols-2">
        <li v-for="classroom in store.items" :key="classroom.id">
          <AppCard :data-testid="`classroom-card-${classroom.id}`">
            <div class="flex flex-col gap-4">
              <div class="space-y-2">
                <div class="flex flex-wrap items-center gap-2">
                  <h2 class="text-lg font-semibold tracking-tight">{{ classroom.name }}</h2>
                  <AppBadge
                    :label="statusLabel(classroom)"
                    :tone="classroom.status === 'OPEN' ? 'positive' : 'muted'"
                    :data-testid="`classroom-status-${classroom.id}`"
                  />
                </div>
                <!-- 描述截断显示：完整内容在详情页，卡片只保留足够辨认的一句话。 -->
                <p
                  v-if="classroom.description"
                  class="line-clamp-2 text-sm leading-relaxed text-ink-muted"
                  :title="classroom.description"
                >
                  {{ classroom.description }}
                </p>
                <p v-else class="text-sm text-ink-muted">未填写说明</p>
              </div>

              <dl class="flex flex-wrap gap-x-6 gap-y-1 text-sm text-ink-muted">
                <div class="flex items-center gap-1.5">
                  <dt>学生</dt>
                  <dd class="font-medium text-ink" :data-testid="`classroom-count-${classroom.id}`">
                    {{ classroom.studentCount }} 人
                  </dd>
                </div>
                <div
                  v-if="classroom.status === 'OPEN'"
                  class="flex items-center gap-1.5"
                  :data-testid="`classroom-run-${classroom.id}`"
                >
                  <dt>本次开始于</dt>
                  <dd class="font-medium text-ink">
                    {{ formatTimeOfDay(classroom.currentRun?.openedAt) }}
                  </dd>
                </div>
              </dl>

              <div class="flex flex-wrap items-center gap-2">
                <AppButton
                  :variant="classroom.status === 'OPEN' ? 'danger' : 'primary'"
                  size="sm"
                  :disabled="store.pendingId !== null"
                  :loading="store.pendingId === classroom.id && toggling"
                  :data-testid="`toggle-${classroom.id}`"
                  @click="openToggleDialog(classroom)"
                >
                  {{ toggleLabel(classroom) }}
                </AppButton>
                <RouterLink
                  :to="{ name: 'teacher-classroom-detail', params: { id: classroom.id } }"
                  class="inline-flex h-8 items-center justify-center rounded-control border border-border-subtle bg-surface px-3 text-xs font-medium text-ink transition-colors hover:bg-surface-muted"
                  :data-testid="`detail-link-${classroom.id}`"
                >
                  查看详情
                </RouterLink>
              </div>
            </div>
          </AppCard>
        </li>
      </ul>
    </template>

    <!--
      二次确认：开启课堂意味着"被授权的学生现在可以进入，并且必须共享整块屏幕"，
      不是一个可以随手点的按钮（teacher.md §5）。dismissible=false 防止误点遮罩丢掉操作。
    -->
    <AppModal
      :open="toggleTarget !== null"
      :tone="toggleTarget?.status === 'OPEN' ? 'danger' : 'default'"
      :title="
        toggleTarget?.status === 'OPEN'
          ? `关闭课堂「${toggleTarget.name}」`
          : `开启课堂「${toggleTarget?.name ?? ''}」`
      "
      :description="
        toggleTarget?.status === 'OPEN'
          ? '关闭后本次课堂运行结束：正在课堂里的学生会断开连接并回到课堂页。关闭不等于删除，随时可以再次开启（会开始新的一次运行）。'
          : '开启后被授权的学生即可进入这个课堂，进入时必须共享整个屏幕，你可以在监督墙看到他们的桌面。'
      "
      :dismissible="false"
      @close="closeToggleDialog"
    >
      <AppAlert v-if="dialogError" tone="danger" data-testid="toggle-dialog-error">
        {{ dialogError }}
      </AppAlert>
      <template #footer>
        <AppButton
          variant="secondary"
          :disabled="store.pendingId !== null"
          @click="closeToggleDialog"
        >
          取消
        </AppButton>
        <AppButton
          :variant="toggleTarget?.status === 'OPEN' ? 'danger' : 'primary'"
          :loading="toggling"
          data-testid="confirm-toggle"
          @click="confirmToggle"
        >
          {{ toggleTarget?.status === 'OPEN' ? '确认关闭' : '确认开启' }}
        </AppButton>
      </template>
    </AppModal>
  </div>
</template>
