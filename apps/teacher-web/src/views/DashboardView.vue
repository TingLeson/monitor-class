<script setup lang="ts">
import { AppAlert, AppButton, AppCard } from '@classwatch/ui'
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { describeClassroomError } from '../lib/classroom-error'
import { useClassroomsStore } from '../stores/classrooms'
import { useSessionStore } from '../stores/session'

/**
 * 工作台（§55 Teacher；docs/frontend/teacher.md §1）。
 *
 * 概览数字**由课堂列表推导**（store 里同一份 items）：不新增统计接口，
 * 也就不会出现"概览说 3 个课堂、列表里只有 2 个"这种两个真相的问题。
 */
const session = useSessionStore()
const store = useClassroomsStore()

/** 进入工作台就刷新一次：老师最常问的"我有几个课堂在开"必须是现在的答案。 */
void store.fetchList()

/**
 * 数字只在成功加载过之后才显示，失败时降级为破折号。
 *
 * WHY 不用 0 兜底：0 是一个明确的事实（"你没有开启中的课堂"），而加载失败时
 * 我们并不知道答案。用 0 冒充事实，会让老师以为课堂被关掉了。
 */
const totalLabel = computed(() => (store.hasLoaded ? String(store.total) : '—'))
const openLabel = computed(() => (store.hasLoaded ? String(store.openCount) : '—'))

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
        <p class="text-xs tracking-wide text-ink-muted uppercase">当前登录用户</p>
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

    <AppCard title="我的课堂" description="这里是你名下课堂的当前情况。">
      <div class="grid gap-6 sm:grid-cols-2">
        <div class="space-y-1">
          <p class="text-xs tracking-wide text-ink-muted uppercase">课堂总数</p>
          <p class="text-3xl font-semibold tracking-tight" data-testid="stat-total">
            {{ totalLabel }}
          </p>
        </div>
        <div class="space-y-1">
          <p class="text-xs tracking-wide text-ink-muted uppercase">开启中</p>
          <p
            class="text-3xl font-semibold tracking-tight"
            :class="store.openCount > 0 ? 'text-status-open' : ''"
            data-testid="stat-open"
          >
            {{ openLabel }}
          </p>
        </div>
      </div>

      <AppAlert v-if="store.error" class="mt-5" tone="danger" data-testid="dashboard-error">
        {{ describeClassroomError(store.error) }}
        <template #actions>
          <AppButton variant="ghost" size="sm" @click="store.fetchList()">重试</AppButton>
        </template>
      </AppAlert>
      <p v-else-if="store.loading" class="mt-5 text-xs text-ink-muted" role="status">
        正在读取课堂列表…
      </p>

      <template #footer>
        <div class="flex flex-wrap items-center gap-2">
          <RouterLink
            :to="{ name: 'teacher-classrooms' }"
            class="inline-flex h-10 items-center justify-center rounded-control border border-border-subtle bg-surface px-4 text-sm font-medium text-ink transition-colors hover:bg-surface-muted"
            data-testid="dashboard-classrooms-link"
          >
            查看我的课堂
          </RouterLink>
          <RouterLink
            :to="{ name: 'teacher-classroom-new' }"
            class="inline-flex h-10 items-center justify-center rounded-control bg-ink px-4 text-sm font-medium text-surface transition-colors hover:bg-ink/90"
            data-testid="dashboard-new-link"
          >
            新建课堂
          </RouterLink>
        </div>
      </template>
    </AppCard>
  </div>
</template>
