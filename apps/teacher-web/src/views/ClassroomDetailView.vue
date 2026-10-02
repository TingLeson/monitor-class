<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="课堂详情"
      description="课堂详情：修改信息、维护授权学生名单、开启或关闭课堂。"
      phase="Phase 3"
      :notes="[
        '§11：只能把 status == ACTIVE 的 STUDENT 账号加入名单；教师只能操作自己的课堂，越权由后端返回 CLASSROOM_NOT_OWNER。',
        '§48 / §49：开启会创建新的 ClassroomRun，关闭会结束当前 Run 并由后端统一收尾在线会话（ROOM_CLOSED）。',
        '§7：只有 CLOSED / OPEN 两个状态，不要引入 PAUSED 等中间态；关闭课堂不等于删除课堂。',
        '§37：前端只隐藏不允许的操作；真正的授权在后端按 owner_teacher_id 判定，任何前端判断都不构成安全边界。',
      ]"
    />
  </div>
</template>
