<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { useRoute } from 'vue-router'

const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-3xl">
    <PhasePlaceholder
      :path="route.path"
      title="课堂中"
      description="课堂内状态页：屏幕共享、摄像头、麦克风与网络状态；不显示自己的屏幕预览。"
      phase="Phase 6"
      :notes="[
        '§56：不要持续显示自己的屏幕 Preview（会形成 screen inside screen），只用小型状态 Indicator 表达「正在共享整个屏幕」。',
        '§22：学生停止共享后必须重新共享整个屏幕才能继续上课（SCREEN_TRACK_ENDED），前端不得静默恢复，也不得降级成窗口共享。',
        'Phase 9 / Phase 10 才接入摄像头与麦克风开关；权限失败分别映射 CAMERA_PERMISSION_DENIED / MIC_PERMISSION_DENIED（§58）。',
        'sessionId 来自 join 响应（§43）；会话状态以后端为准（§12），前端不得凭本地内存推断「已在线」，刷新后必须能凭 sessionId 恢复。',
      ]"
    />
  </div>
</template>
