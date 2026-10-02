<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="我的课堂"
      description="列出当前学生被授权的课堂；只有 OPEN 的课堂可以进入。"
      phase="Phase 4"
      :notes="[
        '§14：只调用 GET /api/v1/student/classrooms。绝对禁止先拉全量课堂再由前端筛选——授权过滤必须发生在后端。',
        '§7：状态只有 OPEN / CLOSED，用 StatusDot 的 open / closed 语义色表达；CLOSED 的卡片显示「暂不可进入」而不是进入按钮。',
        '§37：前端隐藏未授权课堂只是 UX，后端仍必须按 ClassroomStudent 独立校验授权关系。',
        '空状态要区分「没有被分配任何课堂」与「接口失败」：两者对学生的下一步动作完全不同。',
      ]"
    />
  </div>
</template>
