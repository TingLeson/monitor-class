<script setup lang="ts">
import { PhasePlaceholder } from '@classwatch/ui'
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

/**
 * 课堂监督墙（Phase 7 实现）。
 *
 * 这里刻意保持占位：监督墙是整块桌面视频网格 + Focus View（§29 / §30），
 * 它依赖 Phase 6 的媒体链路与 Phase 8 的实时事件；在那些能力就绪之前做出来的
 * 任何"预览"都会变成需要推翻重写的假实现（§53 / §54 的立场一致）。
 *
 * 唯一新增的是"回到课堂详情"的入口：老师从详情页点进来（或直接输 URL）之后，
 * 必须有一条不用按浏览器后退就能回去的路——开关课堂、改名单都在那边。
 */
const route = useRoute()

const classroomId = computed(() => {
  const raw = route.params.id
  return typeof raw === 'string' ? raw : ''
})
</script>

<template>
  <div class="mx-auto flex max-w-3xl flex-col gap-4">
    <div>
      <RouterLink
        v-if="classroomId"
        :to="{ name: 'teacher-classroom-detail', params: { id: classroomId } }"
        class="text-sm text-ink-muted underline-offset-4 transition-colors hover:text-ink hover:underline"
        data-testid="monitor-back"
      >
        ← 返回课堂详情
      </RouterLink>
    </div>

    <PhasePlaceholder
      :path="route.path"
      title="课堂监督墙"
      description="多学生监督墙与 Focus View：学生桌面卡片网格，桌面屏幕是卡片主体、摄像头以画中画呈现，状态色表达屏幕/连接状态。"
      phase="Phase 7：多学生监督墙与 Focus View"
      :notes="[
        '§29 / §30：普通墙用桌面卡片 + 摄像头画中画；Focus View 才使用更高分辨率看一个学生。',
        '§51：卡片必须由后端 Monitor DTO（sessionStatus / screen.active / camera.active / microphone.active / connection）与 LiveKit Track 状态合成，禁止直接把 LiveKit Participant 当业务模型。',
        '§52：autoSubscribe = false，只订阅当前可见或 Focus 的学生轨道，避免同时下载 30 路 1080p。',
        'Phase 8 才接入 WebSocket 业务实时（STUDENT_ONLINE / SCREEN_LOST 等，§47）；在此之前不要用轮询假装实时。',
      ]"
    />
  </div>
</template>
