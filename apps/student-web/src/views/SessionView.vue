<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { RouterLink, useRoute } from 'vue-router'

const route = useRoute()

/**
 * 课堂会话页（§55 / §56）—— **Phase 6 起实现**，本 Phase 仍是占位页。
 *
 * 为什么 Phase 4 只留占位而不做成"看起来在上课"的样子：这一页真正的内容
 * （正在共享整个屏幕、摄像头/麦克风开关、网络状态）全部依赖 §43 的 join 接口与
 * §44 的媒体 Token，而两者都属于 Phase 5/6。现在渲染一组点不动的开关，只会让人
 * 以为系统已经能上课了。
 *
 * 页面现在就把 §56 的界面契约交代清楚，尤其是那条容易被"顺手实现"违背的约束：
 * **不显示自己的屏幕预览**——会把界面变成 screen inside screen inside screen。
 */
</script>

<template>
  <div class="mx-auto flex max-w-3xl flex-col gap-6">
    <PhasePlaceholder
      :path="route.path"
      title="课堂会话 · Phase 6 起实现"
      description="课堂内的状态页：这里会显示「正在共享整个屏幕」、摄像头与麦克风开关、网络状态。"
      phase="Phase 6"
      :notes="[
        '§56：不显示自己的屏幕预览（那会形成 screen inside screen），只用小型状态指示表达「正在共享整个屏幕」。',
        '§22：共享被学生主动停止或意外中断后，必须重新共享整个屏幕才能继续上课（SCREEN_TRACK_ENDED），不得静默恢复，也不得降级成窗口共享。',
        'Phase 9 / Phase 10 才接入摄像头与麦克风开关；权限失败分别对应 CAMERA_PERMISSION_DENIED / MIC_PERMISSION_DENIED（§58）。',
        '§26：这一页也不显示任何其他学生的信息，学生之间互不感知。',
        'sessionId 来自 join 响应（§43）；会话状态以后端为准（§12），前端不得凭内存推断「已在线」，刷新后必须能凭 sessionId 恢复。',
      ]"
    />

    <RouterLink
      :to="{ name: 'student-classrooms' }"
      class="text-sm text-ink-muted underline-offset-4 transition-colors hover:text-ink hover:underline"
      data-testid="back-to-classrooms"
    >
      返回我的课堂
    </RouterLink>
  </div>
</template>
