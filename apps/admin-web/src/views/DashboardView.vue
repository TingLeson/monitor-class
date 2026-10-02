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
      title="管理概览"
      description="管理员工作台：账号体系概览（账号数量、角色分布、停用情况）与用户管理入口。"
      phase="Phase 2"
      :notes="[
        '§68（Phase 2）范围只有用户管理：创建 TEACHER / STUDENT 账号、启用与停用、重置教师密码；课堂业务不在这里。',
        '§4：管理员不参与课堂教学，不应能看到学生屏幕内容或进入监督台。',
        '§37：所有 /api/v1/admin/* 接口都必须由后端校验 role == ADMIN，前端隐藏入口不构成保护。',
        '概览数字必须来自后端聚合接口；不要在浏览器里拉全量用户自行统计（§63 要求列表分页）。',
      ]"
    />
  </div>
</template>
