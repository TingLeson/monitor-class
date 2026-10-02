<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="新建课堂"
      description="填写课堂名称与说明；创建后课堂处于 CLOSED 状态，等待老师开课。"
      phase="Phase 3"
      :notes="[
        '§10：owner_teacher_id 由后端从会话推导，前端禁止提交该字段——接受前端传入的所有者等于允许越权指定课堂归属。',
        '§7：创建后状态必须是 CLOSED，不提供「创建即开启」。',
        '§9 / §11：创建成功后进入课堂详情添加学生，且只能选 role == STUDENT 且 status == ACTIVE 的账号。',
        '名称与说明是学生可见文案，需要长度与空白校验；但校验只是 UX，后端必须独立复核（§63 input validation）。',
      ]"
    />
  </div>
</template>
