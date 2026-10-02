<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

/**
 * 把当前真实 URL 传给占位组件：Phase 0 要能一眼确认"页面显示的路径 == 地址栏路径"，
 * 以后若引入网关前缀或 basename，这行会立刻暴露错位。
 */
const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="管理员登录"
      description="管理员用 account + password 登录，成功后进入账号管理工作台。"
      phase="Phase 1"
      :notes="[
        '§40：POST /api/v1/admin/auth/login 与教师端使用同样的密码认证；§9 规定 ADMIN 的 password_hash 必须非空。',
        '§41：会话凭证只走 HttpOnly + Secure + SameSite Cookie；密码与令牌都不得写入 localStorage。',
        '§5：学生与教师登录在各自入口（student-web / teacher-web）。这一页不做角色分支，也不接受 STUDENT / TEACHER 账号。',
        '§58：AUTH_REQUIRED / ACCOUNT_DISABLED 按错误码映射文案，不把后端原始响应文本直接展示给管理员。',
      ]"
    />
  </div>
</template>
