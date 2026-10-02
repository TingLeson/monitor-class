<script setup lang="ts">
import { isApiError } from '@classwatch/api-client'
import { type ApiErrorCode, apiErrorMessage } from '@classwatch/shared-types'
import { AppButton, AppCard, AppTextField } from '@classwatch/ui'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { safeRedirectTarget } from '../router/guard'
import { useSessionStore } from '../stores/session'

/**
 * 学生登录页（§2.2 / §38 / §58）。
 *
 * 这一页刻意只有**一个**输入框：学生账号无密码、不注册、没有验证码、没有邮箱或
 * 手机号验证（§2.2 逐条列出 ❌）。因此页面上不允许出现密码框、"注册"链接、
 * "忘记密码"——那不是"以后再补"，而是违背已冻结的业务规则。
 * 视图测试会断言 DOM 中不存在 `input[type=password]`。
 *
 * 隐私提示（§57）不放在这里：老师能看到整个屏幕这件事必须在**进入课堂前**
 * 由学生显式确认（Phase 4/5 的 PreJoin 页），登录页只说明"登录本身不会开始共享"。
 */

const session = useSessionStore()
const route = useRoute()
const router = useRouter()

const account = ref('')
const accountError = ref<string | null>(null)
const submitError = ref<string | null>(null)
const submitting = ref(false)

/**
 * 登录后回哪里：优先用守卫塞进 query 的 redirect，但必须过白名单校验。
 * 直接把 query 交给 router.push 就是开放重定向漏洞（详见 guard.ts）。
 */
const redirectTarget = computed(() => safeRedirectTarget(route.query.redirect, '/student/login'))

/**
 * 错误码 → 中文文案（§58：绝不显示后端原始响应文本，更不能显示 500/HTML）。
 *
 * WHY 在这里映射而不是直接用后端 message：后端 message 属于实现细节，
 * 措辞可能变化甚至带上内部信息；用户可见文案必须由前端掌控。
 * 兜底只说"登录失败，请稍后重试"，不透露状态码与原始响应。
 */
const ERROR_MESSAGES: Partial<Record<ApiErrorCode, string>> = {
  // 后端对"账号不存在"与"凭据错误"返回同一个码（反账号枚举），前端不得区分。
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
  return accountError.value === null
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
    await session.login(account.value.trim())
    // 成功后跳回原目标（没有则进首页）。用 replace 而不是 push：
    // 用户按"后退"不该回到一个已经登录状态下的登录页。
    await router.replace(redirectTarget.value ?? { name: 'student-classrooms' })
  } catch (error) {
    submitError.value = describeError(error)
  } finally {
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
      <h1 class="text-2xl font-semibold tracking-tight">学生登录</h1>
      <p class="text-sm leading-relaxed text-ink-muted">
        输入老师发给你的账号即可进入课堂。学生账号没有密码。
      </p>
    </div>

    <!-- 守卫留下的提示（例如"当前账号不是学生账号"）与"服务端未确认登出"共用这一处 -->
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
          placeholder="例如 S10086"
          required
          :disabled="submitting"
          :error="accountError ?? undefined"
          hint="账号由管理员创建；大小写不敏感。"
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

        <AppButton type="submit" block :loading="submitting">进入课堂</AppButton>
      </form>
    </AppCard>

    <p class="text-xs leading-relaxed text-ink-muted">
      登录不会自动开始任何画面共享；进入课堂前你会看到明确的共享范围提示，并由你自己确认。
    </p>
  </div>
</template>
