<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="管理概览"
      description="管理员工作台：账号体系概览（账号数量、角色分布、停用情况）与用户管理入口。"
      phase="Phase 2"
      :notes="[
        '§68（Phase 2）范围只有用户管理：创建 TEACHER / STUDENT 账号、启用与停用、重置教师密码；课堂业务不在这里。',
        '§4：管理员不参与课堂教学，不应能看到学生屏幕内容或进入监督台。',
        '§37：所有 /api/v1/admin/* 接口都必须由后端校验 role == ADMIN，前端隐藏入口不构成保护。',
        '概览数字必须来自后端聚合接口；不要在浏览器里拉全量用户自行统计（§63 要求列表分页）。',
      ]"
    />
  </div>
</template>
