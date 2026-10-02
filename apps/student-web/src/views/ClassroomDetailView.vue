<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="进入课堂"
      description="进入前的 PreJoin 页面：展示课堂信息与隐私提示，再由学生自己发起整个屏幕共享。"
      phase="Phase 4"
      :notes="[
        '§15 Step 1：进入前必须确认已登录、student ACTIVE、属于该课堂、classroom OPEN，任一不满足都不得进入 PreJoin。',
        '§57：请求屏幕前必须显式展示「老师能看到这台显示器上的全部内容，包括其他应用与浏览器」的提示，由学生点击确认——不许偷偷调用 getDisplayMedia。',
        'Phase 5 接入整个屏幕 Gate：displaySurface === \'monitor\' 才通过，window / browser 一律拒绝（§16、§71）。',
        '§18：Screen Gate 通过之后才申请 LiveKit media token，Gate 之前不发任何媒体凭证请求。',
      ]"
    />
  </div>
</template>
