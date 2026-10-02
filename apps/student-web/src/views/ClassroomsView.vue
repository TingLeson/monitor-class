<script setup lang="ts">
import { useSessionStore } from '../stores/session'
import { AppCard, PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
const session = useSessionStore()

/**
 * 当前登录用户区块（Phase 1）。
 *
 * WHY 放在这里：刷新页面后仍能看到自己的名字，是"会话真的生效了"最直接的证据。
 * 它显示的**只有后端返回的字段**——前端不缓存身份，也不据此做任何权限判断
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
    <AppCard data-testid="current-user-card">
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
      title="我的课堂"
      description="列出当前学生被授权的课堂；只有 OPEN 的课堂可以进入。"
      phase="Phase 4"
      :notes="[
        '§14：只调用 GET /api/v1/student/classrooms。绝对禁止先拉全量课堂再由前端筛选——授权过滤必须发生在后端。',
        '§7：状态只有 OPEN / CLOSED，用 StatusDot 的 open / closed 语义色表达；CLOSED 的卡片显示「暂不可进入」而不是进入按钮。',
        '§37：前端隐藏未授权课堂只是 UX，后端仍必须按 ClassroomStudent 独立校验授权关系。',
        '空状态要区分「没有被分配任何课堂」与「接口失败」：两者对学生的下一步动作完全不同。',
      ]"
    />
  </div>
</template>
