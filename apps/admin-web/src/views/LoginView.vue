<script setup lang="ts">
import { isApiError } from '@classwatch/api-client'
import { type ApiErrorCode, apiErrorMessage } from '@classwatch/shared-types'
import { AppButton, AppCard, AppTextField } from '@classwatch/ui'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { safeRedirectTarget } from '../router/guard'
import { useSessionStore } from '../stores/session'

/**
 * 管理员登录页（§39 / §58；管理员见 §40）。
 *
 * 这是密码认证入口：账号 + 密码（`type="password"`、`autocomplete="current-password"`）。
 * 密码只在提交时经过网络，不落任何前端存储（§41）。
 */

const session = useSessionStore()
const route = useRoute()
const router = useRouter()

const account = ref('')
const password = ref('')
const accountError = ref<string | null>(null)
const passwordError = ref<string | null>(null)
const submitError = ref<string | null>(null)
const submitting = ref(false)

/**
 * 登录后回哪里：优先用守卫塞进 query 的 redirect，但必须过白名单校验。
 * 直接把 query 交给 router.push 就是开放重定向漏洞（详见 guard.ts）。
 */
const redirectTarget = computed(() => safeRedirectTarget(route.query.redirect, '/admin/login'))

/**
 * 错误码 → 中文文案（§58：绝不显示后端原始响应文本，更不能显示 500/HTML）。
 *
 * WHY 在这里映射而不是直接用后端 message：后端 message 属于实现细节，
 * 措辞可能变化甚至带上内部信息；用户可见文案必须由前端掌控。
 * 兜底只说"登录失败，请稍后重试"，不透露状态码与原始响应。
 */
const ERROR_MESSAGES: Partial<Record<ApiErrorCode, string>> = {
  // 后端对"账号不存在"与"密码错误"返回同一个码（反账号枚举），前端不得区分，
  // 否则就等于替攻击者确认了"这个账号存在"。
  INVALID_CREDENTIALS: '账号或密码不正确',
  ACCOUNT_DISABLED: '账号已停用，请联系管理员',
  RATE_LIMITED: '尝试过于频繁，请稍后再试',
  CSRF_INVALID: '页面已过期，请刷新页面后重试',
  NETWORK_ERROR: '网络异常，请检查网络后重试',
}

function describeError(error: unknown): string {
  if (isApiError(error)) {
    return ERROR_MESSAGES[error.code] ?? apiErrorMessage(error.code)
  }
  return apiErrorMessage('INTERNAL')
}

function validate(): boolean {
  accountError.value = account.value.trim() ? null : '请输入账号'
  passwordError.value = password.value ? null : '请输入密码'
  return accountError.value === null && passwordError.value === null
}

async function onSubmit(): Promise<void> {
  // 防重复提交：按钮虽然通过 :loading 禁用了，但回车键与"快速点两次"仍可能进来，
  // 这里再挡一次，避免发出两个登录请求（后端按 IP 限流，第二次可能直接 429）。
  if (submitting.value) return
  submitError.value = null
  session.clearNotice()
  if (!validate()) return

  submitting.value = true
  try {
    await session.login(account.value.trim(), password.value)
    // 成功后跳回原目标（没有则进首页）。用 replace 而不是 push：
    // 用户按"后退"不该回到一个已经登录状态下的登录页。
    await router.replace(redirectTarget.value ?? { name: 'admin-dashboard' })
  } catch (error) {
    submitError.value = describeError(error)
  } finally {
    // 无论成败都清空密码：失败后让用户重新输入是正确的安全习惯，
    // 也避免密码长时间停留在内存与 DOM 的 value 里。成功路径上组件即将卸载，
    // 这次赋值没有任何副作用。
    password.value = ''
    submitting.value = false
  }
}

onMounted(async () => {
  // 守卫在重定向过来之前已经尝试过 bootstrap；这里再调一次只在"上次没查成"
  // （例如启动时网络失败）时才会真的发请求，幂等由 store 保证。
  await session.bootstrap()
})
</script>

<template>
  <div class="mx-auto flex max-w-md flex-col gap-6">
    <div class="space-y-2">
      <h1 class="text-2xl font-semibold tracking-tight">管理员登录</h1>
      <p class="text-sm leading-relaxed text-ink-muted">
        使用管理员账号与密码登录，成功后进入账号管理工作台。
      </p>
    </div>

    <!-- 守卫留下的提示（例如"当前账号不是本入口账号"）与"服务端未确认登出"共用这一处 -->
    <p
      v-if="session.notice"
      role="alert"
      data-testid="login-notice"
      class="rounded-control border border-border-subtle bg-surface px-3 py-2 text-sm leading-relaxed text-ink-muted"
    >
      {{ session.notice }}
    </p>

    <AppCard>
      <form class="space-y-5" novalidate @submit.prevent="onSubmit">
        <AppTextField
          v-model="account"
          label="账号"
          autocomplete="username"
          required
          :disabled="submitting"
          :error="accountError ?? undefined"
          @enter="onSubmit"
        />

        <AppTextField
          v-model="password"
          label="密码"
          type="password"
          autocomplete="current-password"
          required
          :disabled="submitting"
          :error="passwordError ?? undefined"
          hint="管理员账号只能由破窗工具创建；忘记密码请联系其他管理员或使用 make create-admin。"
          @enter="onSubmit"
        />

        <p
          v-if="submitError"
          role="alert"
          data-testid="login-error"
          class="rounded-control border border-status-danger/30 bg-status-danger/5 px-3 py-2 text-sm leading-relaxed text-status-danger"
        >
          {{ submitError }}
        </p>

        <AppButton type="submit" block :loading="submitting">进入管理后台</AppButton>
      </form>
    </AppCard>
  </div>
</template>
