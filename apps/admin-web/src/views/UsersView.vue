<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="用户管理"
      description="账号列表：按角色与状态筛选，创建账号，启用或停用账号，重置教师密码。"
      phase="Phase 2"
      :notes="[
        '§9：状态只有 ACTIVE / DISABLED；停用后该账号的会话必须失效（后端 revoke，§41），而不是仅靠前端不再显示。',
        '§9：STUDENT 的 password_hash 必须为 NULL，TEACHER / ADMIN 必须有密码哈希；哈希永远不下发到前端，接口不得返回该字段。',
        '§63：列表接口必须分页，避免一次性把全部账号返回浏览器；敏感操作（停用、重置密码）需要二次确认并以后端返回的新状态为准。',
        '§35：用紧凑列表 + 状态色表达，不要做成满屏表格的传统教务系统。',
      ]"
    />
  </div>
</template>
