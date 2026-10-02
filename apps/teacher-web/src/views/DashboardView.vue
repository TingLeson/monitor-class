<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="工作台"
      description="老师工作台：自己名下课堂的概览，以及开启 / 关闭课堂的入口。"
      phase="Phase 3"
      :notes="[
        '§48 / §49：只有 classroom.owner_teacher_id == 当前老师 才能开启或关闭课堂；ownership 判定必须由后端完成，前端只负责不显示无权操作的按钮。',
        '§7：状态机只有 CLOSED → OPEN → CLOSED；重复操作由后端返回 CLASSROOM_ALREADY_OPEN / CLASSROOM_ALREADY_CLOSED（§58），前端以后端返回的新状态为准。',
        '§42：数据来自 GET /api/v1/teacher/classrooms，禁止请求全量课堂再由前端过滤。',
        '§29：这里只是概览；课堂内的学生桌面网格在 /teacher/classrooms/:id/monitor。',
      ]"
    />
  </div>
</template>
