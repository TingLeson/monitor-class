<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="新建账号"
      description="创建学生账号（无密码）或教师 / 管理员账号（需要初始密码）。"
      phase="Phase 2"
      :notes="[
        '§9：STUDENT 的 password_hash 必须为 NULL，TEACHER / ADMIN 必须有密码——这条约束由后端强制，前端表单只做提示。',
        '密码只在提交时传输，绝不在前端缓存或回显；教师忘记密码走 /reset-password，不提供明文找回。',
        '§68 / §2.2：账号由管理员创建，V1 明确不做学生自助注册。',
        'account 重复必须由后端返回可识别错误（不要靠前端列表去重，列表是分页且可能过期）。',
      ]"
    />
  </div>
</template>
