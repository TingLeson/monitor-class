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
      description="老师名下课堂列表：状态、当前 Run 与学生数，并提供新建课堂入口。"
      phase="Phase 3"
      :notes="[
        '§7：新建出来的课堂状态是 CLOSED，只有老师显式开课才会变 OPEN；不要提供「创建并立即开课」的捷径。',
        '§8：每次 CLOSED → OPEN 都必须产生新的 ClassroomRun（禁止复用旧 Run），历史记录才不会被覆盖。',
        '§11：授权学生名单（ClassroomStudent）在课堂详情页维护，列表页只展示统计数字。',
        '§35：用卡片或紧凑行表达状态，不要做成满屏表格的教务系统风格。',
      ]"
    />
  </div>
</template>
