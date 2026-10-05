<script setup lang="ts">
import type { AdminUserListResponse } from '@classwatch/shared-types'
import { AppAlert, AppButton, AppCard } from '@classwatch/ui'
import { onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { describeAdminError } from '../lib/admin-error'
import { listUsers } from '../lib/admin-users-api'
import { formatDateTime } from '../lib/format'
import { useSessionStore } from '../stores/session'

/**
 * 管理概览（§4 / §55；docs/frontend/admin.md §1）。
 *
 * 职责只有两件：给出账号体系的数量概览，以及通往用户管理的入口。
 * 管理员**不参与课堂**（§4），所以这里不会、也不该出现课堂相关的任何数字或入口。
 */
const session = useSessionStore()

/**
 * 概览口径。
 *
 * WHY 用列表接口的 `total` 而不是在浏览器里拉全量再统计：§63 要求列表分页，
 * 拉全量既慢又会在账号变多后变成一次事故。`pageSize=1` 只取那一行"不需要的数据"
 * 里唯一需要的数字（total），是当前契约下最轻的做法——后端若提供聚合接口，替换的
 * 只有这一处。
 */
type CountKey = 'ADMIN' | 'TEACHER' | 'STUDENT' | 'DISABLED'

const COUNT_TILES: { key: CountKey; label: string; hint: string }[] = [
  { key: 'ADMIN', label: '管理员', hint: '由运维用 adminctl 创建' },
  { key: 'TEACHER', label: '老师', hint: '可创建并管理自己的课堂' },
  { key: 'STUDENT', label: '学生', hint: '免密登录，只能由管理员创建' },
  { key: 'DISABLED', label: '已停用', hint: '全部角色中处于停用状态的账号' },
]

const counts = ref<Record<CountKey, number | null>>({
  ADMIN: null,
  TEACHER: null,
  STUDENT: null,
  DISABLED: null,
})
const countsLoading = ref(false)
const countsError = ref<string | null>(null)

async function loadCounts(): Promise<void> {
  if (countsLoading.value) return
  countsLoading.value = true
  countsError.value = null

  const requests: { key: CountKey; request: Promise<AdminUserListResponse> }[] = [
    { key: 'ADMIN', request: listUsers({ role: 'ADMIN', pageSize: 1 }) },
    { key: 'TEACHER', request: listUsers({ role: 'TEACHER', pageSize: 1 }) },
    { key: 'STUDENT', request: listUsers({ role: 'STUDENT', pageSize: 1 }) },
    { key: 'DISABLED', request: listUsers({ status: 'DISABLED', pageSize: 1 }) },
  ]

  // 并发发出四个轻量请求，并用 allSettled 而不是 all：任何一个失败都不该让整页
  // 变空——概览是辅助信息，拿不到的那一格显示"—"比让管理员看到一片错误页更有用。
  const results = await Promise.allSettled(requests.map((entry) => entry.request))

  const next: Record<CountKey, number | null> = {
    ADMIN: null,
    TEACHER: null,
    STUDENT: null,
    DISABLED: null,
  }
  let firstFailure: unknown = null

  results.forEach((result, index) => {
    const entry = requests[index]
    if (!entry) return
    if (result.status === 'fulfilled') {
      next[entry.key] = result.value.total
    } else if (firstFailure === null) {
      firstFailure = result.reason
    }
  })

  counts.value = next
  if (firstFailure !== null) {
    // 只显示一句可读原因，不暴露是哪一个请求失败的细节（§58）。
    countsError.value = describeAdminError(firstFailure)
  }
  countsLoading.value = false
}

onMounted(() => {
  void loadCounts()
})
</script>

<template>
  <div class="flex flex-col gap-6">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">管理概览</h1>
      <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">
        管理员的职责是账号体系：创建老师与学生、启停账号、重置老师密码。课堂由老师自己管理，管理员不进入课堂。
      </p>
    </header>

    <!--
      当前登录用户（Phase 1）。刷新页面后仍能看到自己的名字，是"会话真的生效了"最直接的证据；
      显示的只有后端返回的字段，不据此做任何权限判断（授权边界在后端，§37）。
    -->
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
          账号 {{ session.user.account }} · 上次登录
          {{
            session.user.lastLoginAt ? formatDateTime(session.user.lastLoginAt) : '本次是首次登录'
          }}
        </p>
        <p v-else class="text-sm text-ink-muted">
          暂时无法连接服务器确认会话；功能恢复后会自动重新确认，不需要手动刷新。
        </p>
      </div>
    </AppCard>

    <AppCard title="常用入口" description="账号管理是管理端唯一的业务范围。">
      <div class="grid gap-3 sm:grid-cols-2">
        <RouterLink
          :to="{ name: 'admin-users' }"
          class="rounded-control border border-border-subtle p-4 transition-colors hover:bg-surface-muted"
          data-testid="entry-users"
        >
          <span class="block text-sm font-medium text-ink">用户管理</span>
          <span class="mt-1 block text-xs leading-relaxed text-ink-muted">
            搜索与筛选账号、编辑显示名、启用或停用、重置老师密码
          </span>
        </RouterLink>
        <RouterLink
          :to="{ name: 'admin-user-new' }"
          class="rounded-control border border-border-subtle p-4 transition-colors hover:bg-surface-muted"
          data-testid="entry-user-new"
        >
          <span class="block text-sm font-medium text-ink">新建账号</span>
          <span class="mt-1 block text-xs leading-relaxed text-ink-muted">
            创建老师（需初始密码）或学生（免密）；管理员只能由运维创建
          </span>
        </RouterLink>
      </div>
    </AppCard>

    <AppCard>
      <template #header>
        <div class="flex w-full items-start justify-between gap-4">
          <div class="space-y-1">
            <h2 class="text-lg font-semibold tracking-tight">账号概览</h2>
            <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">
              这里的数字是各类账号的当前数量，随账号变动自动更新。
            </p>
          </div>
          <AppButton
            variant="secondary"
            size="sm"
            :loading="countsLoading"
            data-testid="refresh-counts"
            @click="loadCounts"
          >
            刷新
          </AppButton>
        </div>
      </template>

      <AppAlert v-if="countsError" class="mb-4" tone="danger" data-testid="counts-error">
        {{ countsError }}
      </AppAlert>

      <dl class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <div
          v-for="tile in COUNT_TILES"
          :key="tile.key"
          class="rounded-control border border-border-subtle p-4"
          :data-testid="`count-${tile.key}`"
        >
          <dt class="text-xs tracking-wide text-ink-muted uppercase">{{ tile.label }}</dt>
          <dd
            class="mt-1 text-2xl font-semibold tracking-tight"
            :class="counts[tile.key] === null ? 'text-ink-muted' : 'text-ink'"
          >
            {{ counts[tile.key] ?? '—' }}
          </dd>
          <p class="mt-1 text-xs leading-relaxed text-ink-muted">{{ tile.hint }}</p>
        </div>
      </dl>
    </AppCard>
  </div>
</template>
