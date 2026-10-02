<script setup lang="ts">
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
import MonitorTile from '../components/MonitorTile.vue'
import { describeClassroomError } from '../lib/classroom-error.ts'
import { useClassroomsStore } from '../stores/classrooms.ts'
import { useMonitorStore } from '../stores/monitor.ts'

/**
 * 课堂监督墙（§29 / §51 / §52）—— Phase 6 版本。
 *
 * 这一版只做一件事：**让老师真的看到学生的桌面**。
 *
 * 数据来自两路，合成卡片时界线必须清楚（§51）：
 * - `GET /teacher/classrooms/:id/monitor`（每 10 秒）：谁在上课、屏幕是否中断 —— 业务状态；
 * - LiveKit 手动订阅：画面有没有到 —— 媒体状态。
 *
 * 刻意**不做**的（都属于后续 Phase，提前做会变成要推翻重写的假实现）：
 * - Focus View（§30）、按可见性/滚动位置动态订阅（§52）→ Phase 7；
 * - 摄像头画中画（§29 的 CAM 角标）→ Phase 9；
 * - 私密语音入口（§31）→ Phase 10；
 * - WebSocket 实时事件（§47）→ Phase 8；本 Phase 用 10 秒轮询，见 store 的说明。
 */
const route = useRoute()
const classrooms = useClassroomsStore()
const monitor = useMonitorStore()

const classroomId = computed(() => String(route.params.id ?? ''))

/** 课堂标题栏的数据来自详情接口（monitor 响应里没有课堂名）。 */
const classroom = computed(() => classrooms.current)

/** 与列表页 / 详情页同一句话：V1 只有 OPEN / CLOSED 两个状态（§7）。 */
function statusLabel(status: 'OPEN' | 'CLOSED'): string {
  return status === 'OPEN' ? '已开启' : '未开启'
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
})

/**
 * 离开页面时断开媒体并停止轮询。
 *
 * 不 await：卸载路径上没人能等；`stop()` 内部的断开是 fire-and-forget，
 * 而本地状态（订阅表、定时器）是同步清掉的。老师关掉监督墙后，
 * LiveKit 那边也会在连接关闭后把订阅一起撤掉。
 */
onBeforeUnmount(() => {
  classrooms.clearDetail()
  void monitor.stop()
})

/** monitor 接口失败（业务面）：保留已有画面，只提示"数据没刷新"。 */
const monitorErrorMessage = computed(() =>
  monitor.error === null ? null : describeClassroomError(monitor.error),
)

const loadingFirstTime = computed(() => !monitor.loaded && monitor.students.length === 0)
</script>

<template>
  <div class="mx-auto flex max-w-6xl flex-col gap-6">
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
        <span class="text-sm text-ink-muted">学生 {{ classroom.studentCount }} 人</span>
      </div>
    </header>

    <!--
      媒体面失败：给明确提示 + 重试入口，绝不白屏。
      业务状态（谁在上课）与媒体状态是两件事，所以这里的提示**不**影响下面的卡片列表。
    -->
    <AppAlert
      v-if="monitor.mediaError"
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
      Phase 6 的监督墙：单列/多列卡片列表。Focus View 与"按可见性动态订阅"属 Phase 7（§30/§52）。
      卡片顺序沿用后端返回的顺序（排序是后端的权威，前端不重排）。
    -->
    <div v-else class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3" data-testid="monitor-grid">
      <MonitorTile
        v-for="student in monitor.students"
        :key="student.studentId"
        :student="student"
        :subscription="monitor.subscriptionOf(student)"
        :media-state="monitor.mediaStateOf(student)"
      />
    </div>
  </div>
</template>
