<script setup lang="ts">
import { isApiError } from '@classwatch/api-client'
import {
  PASSWORD_MIN_LENGTH,
  type AdminUser,
  type Role,
  type UserStatus,
  isRole,
  isUserStatus,
} from '@classwatch/shared-types'
import {
  AppAlert,
  AppBadge,
  AppButton,
  AppCard,
  AppEmptyState,
  AppModal,
  AppPagination,
  AppSelect,
  AppTable,
  AppTextField,
  ProtectedRouteGate,
  type BadgeTone,
  type SelectOption,
} from '@classwatch/ui'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { describeAdminError } from '../lib/admin-error'
import { formatDateTime } from '../lib/format'
import {
  buildUserListQuery,
  parseUserListQuery,
  type UserListQueryState,
} from '../lib/user-list-query'
import { useSessionStore } from '../stores/session'
import { useUsersStore } from '../stores/users'

/**
 * 账号列表页（§4 / §9 / §42 Admin / §68；docs/frontend/admin.md §2）。
 *
 * 三条界面契约：
 * 1. 筛选与分页是 URL 的一部分：刷新、分享、前进后退都能保持视图；
 * 2. 所有写操作先等后端成功，再更新本地（不做乐观更新）；
 * 3. 前端禁用"停用自己"只是为了不制造注定失败的点击——真正的拒绝在后端（§37）。
 */

/** 搜索防抖：太短会每个字符发一次请求，太长会让用户以为界面没反应。 */
const SEARCH_DEBOUNCE_MS = 300

/**
 * 搜索词长度上限，与后端 `q` 的上限（128 字符）一致。
 *
 * WHY 在前端截断而不是等后端拒绝：后端对超长 q 直接返回 400（它要防止超长 ILIKE
 * 模式把数据库拖慢），那会让"粘贴了一整段文字"变成一次加载失败。截断只影响
 * 128 字符之后的输入，而账号与姓名远达不到这个长度。
 */
const SEARCH_MAX_LENGTH = 128

const ROLE_LABELS: Record<Role, string> = { ADMIN: '管理员', TEACHER: '老师', STUDENT: '学生' }
const STATUS_LABELS: Record<UserStatus, string> = { ACTIVE: '启用', DISABLED: '停用' }

/**
 * 角色只用中性层级色（strong/neutral/muted），不复用状态色：
 * 把"管理员"标成绿色会让人读成"在线"，而状态列才是表达 ACTIVE/DISABLED 的地方。
 */
const ROLE_TONES: Record<Role, BadgeTone> = {
  ADMIN: 'strong',
  TEACHER: 'neutral',
  STUDENT: 'muted',
}

const ROLE_FILTER_OPTIONS: SelectOption[] = [
  { value: '', label: '全部角色' },
  { value: 'ADMIN', label: ROLE_LABELS.ADMIN },
  { value: 'TEACHER', label: ROLE_LABELS.TEACHER },
  { value: 'STUDENT', label: ROLE_LABELS.STUDENT },
]

const STATUS_FILTER_OPTIONS: SelectOption[] = [
  { value: '', label: '全部状态' },
  { value: 'ACTIVE', label: STATUS_LABELS.ACTIVE },
  { value: 'DISABLED', label: STATUS_LABELS.DISABLED },
]

const store = useUsersStore()
const session = useSessionStore()
const route = useRoute()
const router = useRouter()

/** 搜索框的本地值：它必须立即回显用户输入，URL/store 由防抖之后才更新。 */
const searchText = ref(store.q)
let searchTimer: ReturnType<typeof setTimeout> | null = null

/** 弹窗里的写操作错误：三个确认弹窗互斥，共用一处展示位即可。 */
const dialogError = ref<string | null>(null)

const renameTarget = ref<AdminUser | null>(null)
const renameValue = ref('')
const renameFieldError = ref<string | null>(null)

const statusTarget = ref<{ user: AdminUser; next: UserStatus } | null>(null)

const resetTarget = ref<AdminUser | null>(null)
const resetPasswordInput = ref('')
const resetPasswordFieldError = ref<string | null>(null)

/**
 * 重置结果：`password` 只可能来自服务端生成的那一次响应。
 * 它只在弹窗打开期间存在于这个 ref 里——关闭即置空，不写 store、不写 Web Storage（§41）。
 */
const resetResult = ref<{ account: string; password: string | null } | null>(null)
const copied = ref(false)
const copyFailed = ref(false)

const hasActiveFilters = computed(
  () => store.q !== '' || store.role !== null || store.status !== null,
)

/** 当前登录管理员自己的 id：用于禁用"停用自己"。 */
const currentUserId = computed(() => session.user?.id ?? null)

function isSelf(user: AdminUser): boolean {
  return currentUserId.value !== null && user.id === currentUserId.value
}

function isJustCreated(user: AdminUser): boolean {
  return store.justCreatedAccount !== null && user.account === store.justCreatedAccount
}

const statusModalTitle = computed(() => {
  const target = statusTarget.value
  if (!target) return ''
  return target.next === 'DISABLED'
    ? `停用账号 ${target.user.account}`
    : `启用账号 ${target.user.account}`
})

const statusModalDescription = computed(() => {
  const target = statusTarget.value
  if (!target) return ''
  return target.next === 'DISABLED'
    ? '停用会立即断开该账号正在进行的课堂，并撤销它的全部会话。该账号将无法登录，直到被重新启用。'
    : '启用后该账号可以重新登录。旧会话不会被恢复，本人需要重新登录一次。'
})

/**
 * URL → store 的唯一入口。immediate 让首屏直接按地址栏里的筛选条件加载，
 * 因此"刷新后保持筛选条件"不需要任何额外的持久化机制。
 */
watch(
  () => route.query,
  async () => {
    await store.applyQuery(parseUserListQuery(route.query))
  },
  { immediate: true },
)

/**
 * store → 搜索框：浏览器前进/后退改了 URL 时输入框必须跟着回退，
 * 否则界面显示的条件与列表内容会对不上。
 *
 * immediate 是必需的：URL 的解析发生在本组件 setup 期间（上面那个 watcher 的
 * immediate 回调），它会在**这个 watcher 注册之前**就把 store.q 改成地址栏里的值；
 * 没有 immediate，搜索框会停在空字符串，而列表已经是筛选后的结果。
 */
watch(
  () => store.q,
  (value) => {
    if (value !== searchText.value) searchText.value = value
  },
  { immediate: true },
)

watch(searchText, (value) => {
  if (searchTimer !== null) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    searchTimer = null
    applySearch(value)
  }, SEARCH_DEBOUNCE_MS)
})

onBeforeUnmount(() => {
  if (searchTimer !== null) clearTimeout(searchTimer)
})

/**
 * 写 URL 用 replace 而不是 push：改筛选条件不是"进入新页面"，
 * 用 push 会让用户按十几次"后退"才能离开列表页。
 */
function navigate(next: UserListQueryState): void {
  void router
    .replace({ name: 'admin-users', query: buildUserListQuery(next) })
    // 导航失败（守卫重定向等）不需要在这里补偿：URL 没变，列表就保持原样。
    // catch 只为避免产生一条未处理的 Promise rejection。
    .catch(() => undefined)
}

function applySearch(value: string): void {
  store.clearCreatedNotice()
  // 搜索变化必须回到第 1 页：停在第 3 页搜一个只有 2 条结果的词，用户会以为搜不到。
  navigate({ ...store.query, q: value.trim().slice(0, SEARCH_MAX_LENGTH), page: 1 })
}

function onRoleChange(value: string): void {
  store.clearCreatedNotice()
  // 用 isRole 校验而不是 `as Role`：下拉的值来自 DOM，和 URL 一样属于外部输入。
  navigate({ ...store.query, role: isRole(value) ? value : null, page: 1 })
}

function onStatusChange(value: string): void {
  store.clearCreatedNotice()
  navigate({ ...store.query, status: isUserStatus(value) ? value : null, page: 1 })
}

function onPageChange(page: number): void {
  navigate({ ...store.query, page })
}

function clearFilters(): void {
  store.clearCreatedNotice()
  searchText.value = ''
  navigate({ q: '', role: null, status: null, page: 1 })
}

function retryList(): void {
  void store.fetchList()
}

/**
 * 写操作失败后的兜底：账号已经不在了（可能被另一个管理员处理掉）时立刻刷新列表，
 * 而不是让用户对着一个不存在的行反复点击。
 */
async function refreshIfUserGone(cause: unknown): Promise<void> {
  if (isApiError(cause) && cause.code === 'USER_NOT_FOUND') await store.fetchList()
}

function openRename(user: AdminUser): void {
  dialogError.value = null
  renameFieldError.value = null
  renameValue.value = user.displayName
  renameTarget.value = user
}

function closeRename(): void {
  renameTarget.value = null
  renameFieldError.value = null
  dialogError.value = null
}

async function submitRename(): Promise<void> {
  const target = renameTarget.value
  if (!target || store.mutating) return

  const displayName = renameValue.value.trim()
  if (!displayName) {
    // 与数据库约束 users_display_name_not_blank 对齐：空白显示名在后端也建不出来。
    renameFieldError.value = '请输入显示名'
    return
  }

  dialogError.value = null
  renameFieldError.value = null
  try {
    await store.updateDisplayName(target.id, displayName)
    closeRename()
  } catch (cause) {
    dialogError.value = describeAdminError(cause)
    await refreshIfUserGone(cause)
  }
}

function openStatusChange(user: AdminUser): void {
  // 按钮已经禁用，这里再挡一次：键盘或脚本触发的点击同样不该发出注定 400 的请求。
  if (isSelf(user)) return
  dialogError.value = null
  statusTarget.value = { user, next: user.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE' }
}

function closeStatusChange(): void {
  statusTarget.value = null
  dialogError.value = null
}

async function confirmStatusChange(): Promise<void> {
  const target = statusTarget.value
  if (!target || store.mutating) return

  dialogError.value = null
  try {
    await store.updateStatus(target.user.id, target.next)
    closeStatusChange()
  } catch (cause) {
    // 后端会拒绝"停用自己"和"停用最后一个管理员"（400 INVALID_REQUEST），
    // 它的 message 比前端通用文案更具体，直接展示（见 describeAdminError）。
    dialogError.value = describeAdminError(cause)
    await refreshIfUserGone(cause)
  }
}

function openReset(user: AdminUser): void {
  dialogError.value = null
  resetPasswordFieldError.value = null
  resetPasswordInput.value = ''
  resetTarget.value = user
}

function closeReset(): void {
  // 立刻清空输入框：它是密码，不该在弹窗关闭后还留在内存里。
  resetPasswordInput.value = ''
  resetPasswordFieldError.value = null
  dialogError.value = null
  resetTarget.value = null
}

async function confirmResetPassword(): Promise<void> {
  const target = resetTarget.value
  if (!target || store.mutating) return

  const password = resetPasswordInput.value
  if (password && password.length < PASSWORD_MIN_LENGTH) {
    resetPasswordFieldError.value = `密码至少 ${PASSWORD_MIN_LENGTH} 位`
    return
  }

  dialogError.value = null
  resetPasswordFieldError.value = null
  try {
    const response = await store.resetTeacherPassword(target.id, password || undefined)
    resetPasswordInput.value = ''
    resetTarget.value = null
    copied.value = false
    copyFailed.value = false
    // 只有服务端生成模式才返回 password；管理员指定密码时用它做一次确认，
    // 不去猜、也不回显管理员刚输入的那个密码。
    resetResult.value = { account: target.account, password: response.password ?? null }
  } catch (cause) {
    if (isApiError(cause) && cause.code === 'PASSWORD_POLICY_VIOLATION') {
      resetPasswordFieldError.value = describeAdminError(cause)
      return
    }
    dialogError.value = describeAdminError(cause)
    await refreshIfUserGone(cause)
  }
}

/** 关闭"一次性密码"弹窗：同时清掉 ref 与复制状态，密码不再留在任何地方。 */
function closeResetResult(): void {
  resetResult.value = null
  copied.value = false
  copyFailed.value = false
}

async function copyOneTimePassword(): Promise<void> {
  const value = resetResult.value?.password
  if (!value) return
  try {
    await navigator.clipboard.writeText(value)
    copied.value = true
    copyFailed.value = false
  } catch {
    // 非安全上下文或浏览器拒绝剪贴板权限：不抛错，提示用户手动选中复制即可。
    copied.value = false
    copyFailed.value = true
  }
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <header class="flex flex-wrap items-end justify-between gap-3">
      <div class="space-y-1">
        <h1 class="text-2xl font-semibold tracking-tight">用户管理</h1>
        <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">
          创建老师与学生账号、编辑显示名、启用或停用账号、重置老师密码。管理员账号由运维用
          <code>make create-admin</code> 创建，不在这里管理。
        </p>
      </div>
      <!-- 用链接而不是 router.push 的按钮：管理端页面也要能被中键/新标签页打开。 -->
      <RouterLink
        :to="{ name: 'admin-user-new' }"
        class="inline-flex h-10 shrink-0 items-center justify-center rounded-control bg-ink px-4 text-sm font-medium text-surface transition-colors hover:bg-ink/90"
      >
        新建账号
      </RouterLink>
    </header>

    <AppAlert v-if="store.justCreatedAccount" tone="success" data-testid="created-notice">
      账号 <span class="font-medium">{{ store.justCreatedAccount }}</span> 已创建。
      <template #actions>
        <AppButton variant="ghost" size="sm" @click="store.clearCreatedNotice()">知道了</AppButton>
      </template>
    </AppAlert>

    <AppCard>
      <div class="grid items-end gap-4 sm:grid-cols-[minmax(0,1fr)_11rem_11rem]">
        <AppTextField
          v-model="searchText"
          label="搜索"
          placeholder="账号或显示名"
          autocomplete="off"
          data-testid="users-search"
        />
        <AppSelect
          :model-value="store.role ?? ''"
          :options="ROLE_FILTER_OPTIONS"
          label="角色"
          data-testid="users-role-filter"
          @update:model-value="onRoleChange"
        />
        <AppSelect
          :model-value="store.status ?? ''"
          :options="STATUS_FILTER_OPTIONS"
          label="状态"
          data-testid="users-status-filter"
          @update:model-value="onStatusChange"
        />
      </div>
      <p v-if="store.loading && store.hasLoaded" class="mt-3 text-xs text-ink-muted" role="status">
        正在更新列表…
      </p>
    </AppCard>

    <AppCard :padded="false">
      <!-- 首次加载：还没有任何数据可显示，用占位而不是空表格。 -->
      <ProtectedRouteGate
        v-if="store.loading && !store.hasLoaded"
        label="正在加载账号列表…"
        hint="如果长时间没有反应，请检查网络连接。"
      />

      <!-- 加载失败与"没有数据"必须长得不一样，否则用户会把故障当成"账号都没了"。 -->
      <AppEmptyState
        v-else-if="store.error"
        tone="danger"
        data-testid="users-error"
        title="账号列表加载失败"
        :description="describeAdminError(store.error)"
      >
        <AppButton variant="secondary" size="sm" @click="retryList">重试</AppButton>
      </AppEmptyState>

      <AppEmptyState
        v-else-if="store.isEmpty && hasActiveFilters"
        data-testid="users-empty-filtered"
        title="没有匹配的账号"
        description="试试调整搜索关键词或筛选条件。"
      >
        <AppButton variant="secondary" size="sm" @click="clearFilters">清除筛选</AppButton>
      </AppEmptyState>

      <AppEmptyState
        v-else-if="store.isEmpty"
        data-testid="users-empty"
        title="还没有任何账号"
        description="创建第一个老师或学生账号后，它会出现在这里。"
      />

      <AppTable v-else label="账号列表">
        <template #head>
          <th scope="col">账号</th>
          <th scope="col">显示名</th>
          <th scope="col">角色</th>
          <th scope="col">状态</th>
          <th scope="col">创建时间</th>
          <th scope="col">上次登录</th>
          <th scope="col" class="text-right">操作</th>
        </template>

        <tr
          v-for="user in store.items"
          :key="user.id"
          :class="isJustCreated(user) ? 'bg-status-open/5' : ''"
          :data-testid="`user-row-${user.id}`"
        >
          <td class="font-medium text-ink">{{ user.account }}</td>
          <td>{{ user.displayName }}</td>
          <td>
            <AppBadge :label="ROLE_LABELS[user.role]" :tone="ROLE_TONES[user.role]" />
          </td>
          <td>
            <AppBadge
              :label="STATUS_LABELS[user.status]"
              :tone="user.status === 'ACTIVE' ? 'positive' : 'muted'"
            />
          </td>
          <td class="whitespace-nowrap text-ink-muted">{{ formatDateTime(user.createdAt) }}</td>
          <td class="whitespace-nowrap text-ink-muted">
            {{ user.lastLoginAt ? formatDateTime(user.lastLoginAt) : '从未登录' }}
          </td>
          <td>
            <div class="flex flex-wrap items-center justify-end gap-2">
              <AppButton
                variant="secondary"
                size="sm"
                :data-testid="`rename-${user.id}`"
                @click="openRename(user)"
              >
                编辑显示名
              </AppButton>

              <!--
                自我保护：停用自己会把当前管理员锁在门外（后端也会拒绝）。
                前端禁用按钮并写明原因，是为了不让用户点到一个注定失败的按钮；
                真正的授权边界仍然在后端（§37 / §63）。
              -->
              <AppButton
                variant="secondary"
                size="sm"
                :disabled="isSelf(user)"
                :title="isSelf(user) ? '不能停用当前登录的管理员账号' : undefined"
                :data-testid="`toggle-status-${user.id}`"
                @click="openStatusChange(user)"
              >
                {{ user.status === 'ACTIVE' ? '停用' : '启用' }}
              </AppButton>
              <span v-if="isSelf(user)" class="text-xs text-ink-muted">不能停用当前登录账号</span>

              <!-- 重置密码只对老师开放：§4 定义的就是"重置老师密码"，管理员走 adminctl。 -->
              <AppButton
                v-if="user.role === 'TEACHER'"
                variant="secondary"
                size="sm"
                :data-testid="`reset-password-${user.id}`"
                @click="openReset(user)"
              >
                重置密码
              </AppButton>
            </div>
          </td>
        </tr>
      </AppTable>
    </AppCard>

    <AppPagination
      v-if="store.hasLoaded && store.total > 0"
      :page="store.page"
      :page-size="store.pageSize"
      :total="store.total"
      unit="个账号"
      @update:page="onPageChange"
    />

    <!-- 编辑显示名：只改显示名，角色与状态各有独立入口（§4）。 -->
    <AppModal
      :open="renameTarget !== null"
      title="编辑显示名"
      :description="
        renameTarget
          ? `账号 ${renameTarget.account}（${ROLE_LABELS[renameTarget.role]}）`
          : undefined
      "
      @close="closeRename"
    >
      <AppTextField
        v-model="renameValue"
        label="显示名"
        autocomplete="off"
        :disabled="store.mutating"
        :error="renameFieldError ?? undefined"
      />
      <AppAlert v-if="dialogError" class="mt-4" tone="danger" data-testid="dialog-error">
        {{ dialogError }}
      </AppAlert>
      <template #footer>
        <AppButton variant="secondary" :disabled="store.mutating" @click="closeRename">
          取消
        </AppButton>
        <AppButton :loading="store.mutating" data-testid="confirm-rename" @click="submitRename">
          保存
        </AppButton>
      </template>
    </AppModal>

    <!-- 启用/停用确认：dismissible=false，避免误点遮罩丢掉这次操作。 -->
    <AppModal
      :open="statusTarget !== null"
      :tone="statusTarget?.next === 'DISABLED' ? 'danger' : 'default'"
      :title="statusModalTitle"
      :description="statusModalDescription"
      :dismissible="false"
      @close="closeStatusChange"
    >
      <AppAlert v-if="dialogError" tone="danger" data-testid="dialog-error">
        {{ dialogError }}
      </AppAlert>
      <template #footer>
        <AppButton variant="secondary" :disabled="store.mutating" @click="closeStatusChange">
          取消
        </AppButton>
        <AppButton
          :variant="statusTarget?.next === 'DISABLED' ? 'danger' : 'primary'"
          :loading="store.mutating"
          data-testid="confirm-status-change"
          @click="confirmStatusChange"
        >
          {{ statusTarget?.next === 'DISABLED' ? '确认停用' : '确认启用' }}
        </AppButton>
      </template>
    </AppModal>

    <!-- 重置老师密码：留空由服务端生成高强度密码（不传 password）。 -->
    <AppModal
      :open="resetTarget !== null"
      tone="danger"
      title="重置老师密码"
      :description="resetTarget ? `重置会撤销 ${resetTarget.displayName} 的全部会话。` : undefined"
      :dismissible="false"
      @close="closeReset"
    >
      <AppTextField
        v-model="resetPasswordInput"
        label="新密码（可留空）"
        type="password"
        autocomplete="new-password"
        :disabled="store.mutating"
        :error="resetPasswordFieldError ?? undefined"
        :hint="`至少 ${PASSWORD_MIN_LENGTH} 位；留空则由服务端生成一个高强度密码`"
      />
      <AppAlert v-if="dialogError" class="mt-4" tone="danger" data-testid="dialog-error">
        {{ dialogError }}
      </AppAlert>
      <template #footer>
        <AppButton variant="secondary" :disabled="store.mutating" @click="closeReset"
          >取消</AppButton
        >
        <AppButton
          variant="danger"
          :loading="store.mutating"
          data-testid="confirm-reset-password"
          @click="confirmResetPassword"
        >
          确认重置
        </AppButton>
      </template>
    </AppModal>

    <!--
      一次性密码：只有服务端生成模式才有 password。
      关闭时把 resetResult 置空，这段字符串会跟着 v-if 一起离开 DOM 与内存。
    -->
    <AppModal
      :open="resetResult !== null"
      title="请转达新密码"
      :description="
        resetResult?.password
          ? `账号 ${resetResult.account} 的密码已重置，本密码只显示这一次。`
          : `账号 ${resetResult?.account ?? ''} 的密码已按你输入的内容重置。`
      "
      @close="closeResetResult"
    >
      <div v-if="resetResult?.password" class="space-y-3">
        <p
          class="rounded-control border border-border-subtle bg-surface-muted px-3 py-2 font-mono text-sm break-all select-all"
          data-testid="one-time-password"
        >
          {{ resetResult.password }}
        </p>
        <div class="flex items-center gap-3">
          <AppButton
            variant="secondary"
            size="sm"
            data-testid="copy-password"
            @click="copyOneTimePassword"
          >
            复制密码
          </AppButton>
          <span v-if="copied" class="text-xs text-status-open" data-testid="copy-result">
            已复制
          </span>
          <span v-else-if="copyFailed" class="text-xs text-ink-muted" data-testid="copy-result">
            复制失败，请手动选中复制
          </span>
        </div>
        <p class="text-xs leading-relaxed text-status-danger">
          请立即通过安全渠道转达给该老师。本密码不会再次显示，关闭后不再保留。
        </p>
      </div>
      <p v-else class="text-sm leading-relaxed text-ink-muted">
        请把新密码直接转达给该老师；重置已撤销其全部会话。
      </p>
      <template #footer>
        <AppButton data-testid="close-one-time-password" @click="closeResetResult">
          我已转达，关闭
        </AppButton>
      </template>
    </AppModal>
  </div>
</template>
