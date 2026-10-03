<script setup lang="ts">
import { AppButton, AppCard, AppEmptyState, ProtectedRouteGate, StatusDot } from '@classwatch/ui'
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import {
  canEnterClassroom,
  classroomStatusLabel,
  classroomStatusTone,
} from '../lib/classroom-status'
import { formatTimeOfDay } from '../lib/format'
import { describeRealtimeStatus } from '../lib/realtime-status'
import { describeStudentClassroomError } from '../lib/student-classroom-error'
import { useClassroomsStore } from '../stores/classrooms'
import { useRealtimeStore } from '../stores/realtime'

/**
 * 我的课堂（§14；docs/frontend/student.md §2）。
 *
 * 这个页面上只有两类信息：这是哪门课、现在能不能进。三件事必须同时成立：
 *
 * 1. **列表由后端过滤**。这里调用的接口只返回"我被 ClassroomStudent 授权的课堂"，
 *    前端没有任何筛选逻辑，也拿不到别人的课堂（§14 / §37）。
 * 2. **CLOSED 的课堂照样显示**。未开启就消失，学生会以为"老师没把我加进去"，
 *    然后去找老师——而实际上只是还没到开课时间（docs/frontend/student.md §2.3）。
 * 3. **不显示任何关于其他学生的信息**。§26 要求学生之间完全隔离：没有"班级人数"、
 *    没有名单、没有"谁在线"。DTO 里根本没有这些字段，界面也不许旁敲侧击地补出来。
 *
 * Phase 8 起列表还能**自己变**：老师开课时服务端推 `ROOM_OPENED`，卡片上的
 * 「进入课堂」会自动变可用（§47/§48），学生不必再刷新页面去等一个可能已经过期的状态。
 */
const store = useClassroomsStore()
const realtime = useRealtimeStore()

/**
 * 进入页面就刷新一次。
 *
 * WHY 不只在 hasLoaded 为 false 时加载：课堂的开启/关闭由老师随时触发，
 * "我看到的到底是不是现在的状态"是这一页唯一重要的事——学生按着过期的"已开启"
 * 点进 PreJoin，只会得到一次"老师尚未开启本课堂"。
 */
void store.fetchList()

/**
 * 实时通道状态（§47）：只在**没连上**时显示。
 *
 * 连上时什么都不说——列表能自己更新是正常状态，不需要一行常驻的"已连接"占地方；
 * 没连上时必须说，因为这一页的时效性全部建立在实时事件之上（兜底只有手动刷新）。
 */
const realtimeDisplay = computed(() =>
  describeRealtimeStatus(realtime.state, {
    authFailed: realtime.authFailed,
    wasConnected: realtime.wasConnected,
  }),
)
</script>

<template>
  <div class="flex flex-col gap-6">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">我的课堂</h1>
      <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">
        这里只显示老师把你加入的课堂。进入课堂需要共享整个电脑屏幕，请先确认你已经准备好。
      </p>
      <!-- 通道没连上时才出现：老师开课的推送收不到，列表就不会自己更新。 -->
      <p
        v-if="realtime.started && realtime.state !== 'open'"
        class="flex flex-wrap items-center gap-x-2 text-xs leading-relaxed text-ink-muted"
        data-testid="classrooms-realtime"
        :data-realtime-state="realtime.state"
      >
        <StatusDot :status="realtimeDisplay.tone" :label="realtimeDisplay.label" />
        <span v-if="realtimeDisplay.hint">{{ realtimeDisplay.hint }}</span>
      </p>
    </header>

    <!-- 首次加载：还没有任何数据可显示，用占位而不是"还没有课堂"。 -->
    <ProtectedRouteGate
      v-if="store.loading && !store.hasLoaded"
      label="正在加载你的课堂…"
      hint="如果长时间没有反应，请检查网络连接。"
    />

    <!-- 加载失败与"没有被安排课堂"必须长得不一样，否则学生会把故障当成"我被移出名单了"。 -->
    <AppCard v-else-if="store.error">
      <AppEmptyState
        tone="danger"
        data-testid="classrooms-error"
        title="课堂列表加载失败"
        :description="describeStudentClassroomError(store.error)"
      >
        <AppButton variant="secondary" size="sm" @click="store.fetchList()">重试</AppButton>
      </AppEmptyState>
    </AppCard>

    <AppCard v-else-if="store.isEmpty">
      <AppEmptyState
        data-testid="classrooms-empty"
        title="还没有被加入任何课堂"
        description="老师把你加入课堂后，这里就会出现。如果你确定应该已经加入，请联系老师确认账号是否正确。"
      />
    </AppCard>

    <template v-else>
      <p v-if="store.loading" class="text-xs text-ink-muted" role="status">正在更新课堂…</p>

      <ul class="grid gap-4 md:grid-cols-2">
        <li v-for="classroom in store.items" :key="classroom.id">
          <AppCard :data-testid="`classroom-card-${classroom.id}`">
            <div class="flex flex-col gap-4">
              <div class="space-y-2">
                <h2 class="text-lg font-semibold tracking-tight">{{ classroom.name }}</h2>
                <!-- 老师填写的说明：与详情页显示的是同一份内容，卡片上截断成两行。 -->
                <p
                  v-if="classroom.description"
                  class="line-clamp-2 text-sm leading-relaxed text-ink-muted"
                  :title="classroom.description"
                >
                  {{ classroom.description }}
                </p>
                <p
                  class="text-sm text-ink-muted"
                  :data-testid="`classroom-teacher-${classroom.id}`"
                >
                  {{ classroom.teacher.displayName }}
                </p>
                <StatusDot
                  :status="classroomStatusTone(classroom.status)"
                  :label="classroomStatusLabel(classroom.status)"
                  :data-testid="`classroom-status-${classroom.id}`"
                />
                <!--
                  "本次开始于" 只在课堂真的开着、且后端下发了 currentRun 时出现。
                  CLOSED 的课堂谈"开始于几点"没有意义，也绝不会显示成一行破折号。
                -->
                <p
                  v-if="classroom.status === 'OPEN' && classroom.currentRun"
                  class="text-xs text-ink-muted"
                  :data-testid="`classroom-run-${classroom.id}`"
                >
                  本次开始于 {{ formatTimeOfDay(classroom.currentRun.openedAt) }}
                </p>
              </div>

              <div class="flex justify-end">
                <!--
                  OPEN → 进入 PreJoin（Phase 4 里"进入课堂"就到此为止，不连媒体服务器）。
                  用 RouterLink 而不是 router.push 的按钮：课堂入口应当能被中键/新标签页打开。
                -->
                <RouterLink
                  v-if="canEnterClassroom(classroom.status)"
                  :to="{ name: 'student-classroom-detail', params: { id: classroom.id } }"
                  class="inline-flex h-10 items-center justify-center rounded-control bg-ink px-4 text-sm font-medium text-surface transition-colors hover:bg-ink/90"
                  :data-testid="`enter-${classroom.id}`"
                >
                  进入课堂
                </RouterLink>
                <!--
                  CLOSED → 按钮保持可见但禁用，文案说明"现在不行"而不是"操作失败"。
                  把它隐藏起来等于把"这节课存在但还没开始"这个事实也一起藏掉（§14）。
                -->
                <AppButton
                  v-else
                  variant="secondary"
                  disabled
                  :data-testid="`enter-${classroom.id}`"
                >
                  暂不可进入
                </AppButton>
              </div>
            </div>
          </AppCard>
        </li>
      </ul>
    </template>
  </div>
</template>
