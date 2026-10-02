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
      title="学生登录"
      description="学生输入账号（无需密码）登录，成功后进入「我的课堂」。"
      phase="Phase 1"
      :notes="[
        '§38：POST /api/v1/student/auth/login 的请求体只有 account。学生账号无密码是明确的业务规则，不要把它包装成强身份认证。',
        '§41：登录成功后由后端下发 HttpOnly + Secure + SameSite 会话 Cookie；前端不得把任何长期凭证写入 localStorage。',
        '§5：教师与管理员登录在各自入口（teacher-web / admin-web）。这一页永远不做角色分支，也不接受 TEACHER / ADMIN 账号登录。',
        '§58：失败提示按错误码映射（AUTH_REQUIRED / ACCOUNT_DISABLED），不把后端原始响应文本直接展示给学生。',
      ]"
    />
  </div>
</template>
