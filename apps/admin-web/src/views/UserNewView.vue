<script setup lang="ts">
import { isApiError } from '@classwatch/api-client'
import {
  ADMIN_CREATABLE_ROLES,
  PASSWORD_MIN_LENGTH,
  type AdminCreatableRole,
} from '@classwatch/shared-types'
import { AppAlert, AppButton, AppCard, AppTextField } from '@classwatch/ui'
import { ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { createUserErrorField, describeAdminError } from '../lib/admin-error'
import { useUsersStore } from '../stores/users'

/**
 * 新建账号页（§2.2 / §4 / §9 / §68）。
 *
 * 两条必须写在界面上的业务规则：
 * 1. **没有"管理员"选项**，并且说明原因——否则管理员会以为是系统坏了；
 * 2. **学生没有密码框**，并且说明这是业务规则而不是漏做的功能。
 */
const router = useRouter()
const store = useUsersStore()

/**
 * 账号格式与数据库约束 users_account_format 一致（`^[A-Za-z0-9._-]{3,64}$`）。
 *
 * WHY 前端也要校验：账号是学生唯一的凭据，格式（禁止空白与同形字符）本身是安全边界；
 * 在这里挡一次能让用户在提交前就知道规则，但**唯一执行点仍是后端与数据库**——
 * 前端校验永远不是安全措施（§37 / §63）。
 */
const ACCOUNT_PATTERN = /^[A-Za-z0-9._-]{3,64}$/

const ROLE_HINTS: Record<AdminCreatableRole, { label: string; hint: string }> = {
  STUDENT: { label: '学生', hint: '只需账号，登录时不输入密码（§2.2）' },
  TEACHER: { label: '老师', hint: '账号 + 初始密码，可创建并管理自己的课堂' },
}

/**
 * 角色选项从 ADMIN_CREATABLE_ROLES 派生，而不是在页面里手写一个数组：
 * 选项与契约里的"可创建角色"是同一份数据，契约变化时前端不会多出一个后端并不接受
 * 的选项（当前它保证页面上**不存在** ADMIN，见 §4）。
 */
const ROLE_CHOICES = ADMIN_CREATABLE_ROLES.map((value) => ({ value, ...ROLE_HINTS[value] }))

const role = ref<AdminCreatableRole>('STUDENT')
const account = ref('')
const displayName = ref('')
const password = ref('')

const accountError = ref<string | null>(null)
const displayNameError = ref<string | null>(null)
const passwordError = ref<string | null>(null)
/** 顶部错误：没有对应输入框的错误（限流、权限、网络、后端拒绝的其它原因）。 */
const submitError = ref<string | null>(null)
const submitting = ref(false)

/**
 * 切换角色时清空密码与它的错误。
 *
 * WHY 不能只是隐藏密码框：隐藏的字段会把密码留在内存与组件状态里，切回"老师"时
 * 还容易被当成新密码直接提交；而属于另一个角色的错误提示留着只会误导用户。
 */
watch(role, () => {
  password.value = ''
  passwordError.value = null
  submitError.value = null
})

/** 本地校验：错误文案挂到对应字段上（后端仍会独立校验一遍）。 */
function validate(): boolean {
  const trimmedAccount = account.value.trim()
  if (!trimmedAccount) {
    accountError.value = '请输入账号'
  } else if (!ACCOUNT_PATTERN.test(trimmedAccount)) {
    accountError.value = '账号只能包含字母、数字与 . _ -，长度 3–64 位'
  } else {
    accountError.value = null
  }

  displayNameError.value = displayName.value.trim() ? null : '请输入显示名'

  if (role.value === 'TEACHER') {
    if (!password.value) {
      passwordError.value = '请输入初始密码'
    } else if (password.value.length < PASSWORD_MIN_LENGTH) {
      passwordError.value = `密码至少 ${PASSWORD_MIN_LENGTH} 位`
    } else if (password.value.trim().toLowerCase() === trimmedAccount.toLowerCase()) {
      // 后端也禁止"密码等于账号"（大小写不敏感，因为账号是 citext）；
      // 提前提示比让用户白等一次 400 更好。
      passwordError.value = '密码不能与账号相同'
    } else {
      passwordError.value = null
    }
  } else {
    passwordError.value = null
  }

  return (
    accountError.value === null && displayNameError.value === null && passwordError.value === null
  )
}

/**
 * 服务端错误 → 字段 / 顶部提示（§58）。
 *
 * 只有契约里明确属于某个输入框的码才挂到字段上：ACCOUNT_ALREADY_EXISTS → 账号，
 * PASSWORD_POLICY_VIOLATION → 密码；其余（INVALID_REQUEST、限流、权限、网络）走顶部，
 * 因为把"请求被拒绝"塞进账号框，只会让用户盯着一个没有问题的输入框找原因。
 */
function applyServerError(cause: unknown): void {
  const message = describeAdminError(cause)
  if (!isApiError(cause)) {
    submitError.value = message
    return
  }

  switch (createUserErrorField(cause.code)) {
    case 'account':
      accountError.value = message
      return
    case 'password':
      passwordError.value = message
      return
    default:
      submitError.value = message
  }
}

async function onSubmit(): Promise<void> {
  // 防重复提交：按钮已按 loading 禁用，但回车键与快速双击仍可能触发第二次提交。
  if (submitting.value || store.mutating) return
  submitError.value = null
  if (!validate()) return

  submitting.value = true
  try {
    const trimmedAccount = account.value.trim()
    const trimmedName = displayName.value.trim()

    if (role.value === 'TEACHER') {
      await store.createUser({
        account: trimmedAccount,
        displayName: trimmedName,
        role: 'TEACHER',
        password: password.value,
      })
    } else {
      // 学生分支在类型上就没有 password 字段：§2.2 的"学生免密"不是靠"记得不要传"
      // 来保证的，而是写不出来的。
      await store.createUser({
        account: trimmedAccount,
        displayName: trimmedName,
        role: 'STUDENT',
      })
    }

    // 用 replace：创建完再按"后退"回到一个已提交的表单，容易变成重复创建。
    // "新建的账号是谁"由 store 的 justCreatedAccount 带给列表页做提示与高亮。
    //
    // 导航失败单独吞掉：账号**已经创建成功**，把一次导航异常报成"创建失败"会让管理员
    // 以为要重来一次，然后撞上 ACCOUNT_ALREADY_EXISTS。
    await router.replace({ name: 'admin-users' }).catch(() => undefined)
  } catch (cause) {
    applyServerError(cause)
  } finally {
    // 无论成败都清空密码：失败后重新输入是正确的安全习惯，也避免它长期留在内存与 DOM 里。
    password.value = ''
    submitting.value = false
  }
}
</script>

<template>
  <div class="mx-auto flex max-w-2xl flex-col gap-6">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">新建账号</h1>
      <p class="text-sm leading-relaxed text-ink-muted">
        账号由管理员创建，V1 不提供任何自助注册入口（§2.2 / §68）。
      </p>
    </header>

    <AppCard>
      <form class="space-y-6" novalidate @submit.prevent="onSubmit">
        <fieldset class="space-y-2">
          <legend class="text-sm font-medium text-ink">角色</legend>
          <div class="grid gap-3 sm:grid-cols-2">
            <label
              v-for="choice in ROLE_CHOICES"
              :key="choice.value"
              class="flex cursor-pointer items-start gap-3 rounded-control border p-3 transition-colors"
              :class="
                role === choice.value
                  ? 'border-status-open bg-status-open/5'
                  : 'border-border-subtle hover:bg-surface-muted'
              "
              :data-testid="`role-option-${choice.value}`"
            >
              <input
                v-model="role"
                type="radio"
                name="role"
                :value="choice.value"
                :disabled="submitting"
                class="mt-1"
              />
              <span class="space-y-0.5">
                <span class="block text-sm font-medium text-ink">{{ choice.label }}</span>
                <span class="block text-xs leading-relaxed text-ink-muted">{{ choice.hint }}</span>
              </span>
            </label>
          </div>
        </fieldset>

        <!-- 说明为什么没有管理员选项：不说清楚，管理员会以为是系统缺功能（§4）。 -->
        <AppAlert tone="info" data-testid="no-admin-notice">
          这里没有"管理员"选项，后端也会拒绝创建管理员：§4 只允许运维在服务器上执行
          <code>make create-admin</code> 创建管理员。管理端接口若能造管理员，一个被盗的管理员会话
          就能留下一个永久后门。
        </AppAlert>

        <AppTextField
          v-model="account"
          label="账号"
          autocomplete="off"
          required
          :disabled="submitting"
          :error="accountError ?? undefined"
          hint="学生用账号登录，请发给本人；只允许字母、数字与 . _ -，长度 3–64 位"
          data-testid="account-field"
        />

        <AppTextField
          v-model="displayName"
          label="显示名"
          autocomplete="off"
          required
          :disabled="submitting"
          :error="displayNameError ?? undefined"
          hint="姓名或称呼，会显示在课堂与监督界面里"
          data-testid="display-name-field"
        />

        <AppTextField
          v-if="role === 'TEACHER'"
          v-model="password"
          label="初始密码"
          type="password"
          autocomplete="new-password"
          required
          :disabled="submitting"
          :error="passwordError ?? undefined"
          :hint="`至少 ${PASSWORD_MIN_LENGTH} 位，且不能与账号相同；创建后请通过安全渠道转达给该老师`"
          data-testid="password-field"
        />

        <!-- 学生免密是业务规则，必须写出来：否则会被当成"忘了做密码框"。 -->
        <AppAlert v-else tone="info" data-testid="student-no-password-notice">
          学生账号不设置密码：学生登录时只输入账号（§2.2 明确的业务规则，不是缺功能）。
          因此这里没有密码框，后端也会拒绝"带密码的学生"。
        </AppAlert>

        <AppAlert v-if="submitError" tone="danger" data-testid="submit-error">
          {{ submitError }}
        </AppAlert>

        <div class="flex flex-wrap items-center gap-4">
          <AppButton type="submit" :loading="submitting">创建账号</AppButton>
          <RouterLink
            :to="{ name: 'admin-users' }"
            class="text-sm text-ink-muted transition-colors hover:text-ink"
          >
            返回账号列表
          </RouterLink>
        </div>
      </form>
    </AppCard>
  </div>
</template>
