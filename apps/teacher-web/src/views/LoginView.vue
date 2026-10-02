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
      title="教师登录"
      description="教师用 account + password 登录，成功后进入工作台。"
      phase="Phase 1"
      :notes="[
        '§39：POST /api/v1/teacher/auth/login 由后端校验 password hash；§9 规定 TEACHER 的 password_hash 必须非空。',
        '§41：会话凭证只走 HttpOnly + Secure + SameSite Cookie；密码与令牌都不得写入 localStorage 或前端持久化存储。',
        '§5：学生与管理员登录在各自入口（student-web / admin-web）。这一页不做角色分支，也不接受 STUDENT / ADMIN 账号。',
        '§58：AUTH_REQUIRED / ACCOUNT_DISABLED 按错误码映射文案，不把后端原始响应文本直接展示给老师。',
      ]"
    />
  </div>
</template>
