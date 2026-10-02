<script setup lang="ts">
import { AppCard, PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'
import { useSessionStore } from '../stores/session'

const route = useRoute()
const session = useSessionStore()

/**
 * 当前登录用户区块（Phase 1）。
 *
 * WHY 放在工作台：刷新页面后仍能看到自己的名字，是"会话真的生效了"最直接的证据。
 * 显示的**只有后端返回的字段**——前端不缓存身份，也不据此做任何权限判断
 * （授权边界在后端，§37）。
 */
function formatLastLogin(value: string | null | undefined): string {
  if (!value) return '本次是首次登录'
  // 保留字符串形态、只在展示层格式化（见 shared-types 的 IsoDateTime 说明）。
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? '—' : parsed.toLocaleString()
}
</script>

<template>
  <div class="mx-auto flex max-w-3xl flex-col gap-6">
    <AppCard>
      <div class="space-y-1">
        <p class="text-xs uppercase tracking-wide text-ink-muted">当前登录用户</p>
        <p
          v-if="session.user"
          class="text-xl font-semibold tracking-tight"
          data-testid="current-user-name"
        >
          {{ session.user.displayName }}
        </p>
        <p
          v-else
          class="text-xl font-semibold tracking-tight text-ink-muted"
          data-testid="current-user-unknown"
        >
          尚未确认
        </p>
        <p v-if="session.user" class="text-sm text-ink-muted">
          账号 {{ session.user.account }} · 上次登录 {{ formatLastLogin(session.user.lastLoginAt) }}
        </p>
        <p v-else class="text-sm text-ink-muted">
          暂时无法连接服务器确认会话；功能恢复后会自动重新确认，不需要手动刷新。
        </p>
      </div>
    </AppCard>

    <PhasePlaceholder
      :path="route.path"
      title="工作台"
      description="老师工作台：自己名下课堂的概览，以及开启 / 关闭课堂的入口。"
      phase="Phase 3"
      :notes="[
        '§48 / §49：只有 classroom.owner_teacher_id == 当前老师 才能开启或关闭课堂；ownership 判定必须由后端完成，前端只负责不显示无权操作的按钮。',
        '§7：状态机只有 CLOSED → OPEN → CLOSED；重复操作由后端返回 CLASSROOM_ALREADY_OPEN / CLASSROOM_ALREADY_CLOSED（§58），前端以后端返回的新状态为准。',
        '§42：数据来自 GET /api/v1/teacher/classrooms，禁止请求全量课堂再由前端过滤。',
        '§29：这里只是概览；课堂内的学生桌面网格在 /teacher/classrooms/:id/monitor。',
      ]"
    />
  </div>
</template>
