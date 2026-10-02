<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="课堂监督台"
      description="学生桌面卡片网格：桌面屏幕是卡片主体，摄像头以画中画呈现，状态色表达屏幕/连接状态。"
      phase="Phase 7"
      :notes="[
        '§51：卡片必须由后端 Monitor DTO（sessionStatus / screen.active / camera.active / microphone.active / connection）与 LiveKit Track 状态合成，禁止直接把 LiveKit Participant 当业务模型。',
        '§52：autoSubscribe = false，只订阅当前可见或 Focus 的学生轨道，避免同时下载 30 路 1080p。',
        '§29 / §30：桌面为卡片主体、摄像头画中画；Focus View 才使用更高分辨率。',
        'Phase 8 才接入 WebSocket 业务实时（STUDENT_ONLINE / SCREEN_LOST 等，§47）；在此之前不要用轮询假装实时。',
      ]"
    />
  </div>
</template>
