<script setup lang="ts">
import { isApiError } from '@classwatch/api-client'
import {
  CLASSROOM_DESCRIPTION_MAX_LENGTH,
  CLASSROOM_NAME_MAX_LENGTH,
} from '@classwatch/shared-types'
import { AppAlert, AppButton, AppCard, AppTextArea, AppTextField } from '@classwatch/ui'
import { computed, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { classroomErrorField, describeClassroomError } from '../lib/classroom-error'
import { validateClassroomDescription, validateClassroomName } from '../lib/classroom-form'
import { useClassroomsStore } from '../stores/classrooms'

/**
 * 新建课堂（§7 / §42 Teacher；docs/frontend/teacher.md §3）。
 *
 * 关键产品规则：创建 ≠ 开课。新课堂一律是 CLOSED（§7），因此这里没有状态开关，
 * 也没有"创建并立即开课"的捷径——开课是一个需要二次确认的独立动作（§48）。
 */
const store = useClassroomsStore()
const router = useRouter()

const name = ref('')
const description = ref('')
const nameError = ref<string | null>(null)
const descriptionError = ref<string | null>(null)
/** 与具体字段无关的失败（网络、权限、服务端 500）。 */
const submitError = ref<string | null>(null)

/**
 * 实时字数按**去空白**后计算：后端规则就是"去空白后 1–80 / ≤500"（§7），
 * 用原始长度显示会让"打了几个空格就变红"这种假告警出现。
 */
const nameLength = computed(() => name.value.trim().length)
const descriptionLength = computed(() => description.value.trim().length)

async function submit(): Promise<void> {
  if (store.creating) return

  submitError.value = null
  nameError.value = validateClassroomName(name.value)
  descriptionError.value = validateClassroomDescription(description.value)
  if (nameError.value || descriptionError.value) return

  try {
    const created = await store.create({
      name: name.value.trim(),
      // 空说明按契约存成 null（"空串视为未填写"，§7）。
      description: description.value.trim() || null,
    })
    await router.push({ name: 'teacher-classroom-detail', params: { id: created.id } })
  } catch (cause) {
    /*
     * 服务端校验失败必须挂到"用户正在填的那个框"上，但契约里没有字段级错误码：
     * 后端对名称/说明的违规统一返回 INVALID_REQUEST（400），而它同时覆盖未知字段、
     * 请求体形状等原因。因此 classroomErrorField 只把"账号类"错误归到输入框，
     * 其余走顶部提示 + 后端 message（describeClassroomError 会优先采用已本地化的
     * 后端说明），而不是把一句笼统的拒绝塞进名称框让老师反复改一个没问题的字段。
     */
    const code = isApiError(cause) ? cause.code : null
    const field = code === null ? null : classroomErrorField(code)
    if (field === 'name') nameError.value = describeClassroomError(cause)
    else if (field === 'description') descriptionError.value = describeClassroomError(cause)
    else submitError.value = describeClassroomError(cause)
  }
}
</script>

<template>
  <div class="mx-auto flex max-w-2xl flex-col gap-6">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">新建课堂</h1>
      <p class="max-w-2xl text-sm leading-relaxed text-ink-muted">
        创建后课堂处于未开启状态；添加学生名单、确认无误后再开启。
      </p>
    </header>

    <AppCard>
      <form class="flex flex-col gap-5" @submit.prevent="submit">
        <AppTextField
          v-model="name"
          label="课堂名称"
          placeholder="例如：C++ 算法训练"
          autocomplete="off"
          required
          :disabled="store.creating"
          :error="nameError ?? undefined"
          :hint="`${nameLength} / ${CLASSROOM_NAME_MAX_LENGTH}`"
          data-testid="classroom-name"
        />

        <AppTextArea
          v-model="description"
          label="课堂说明（可不填）"
          placeholder="例如：第三章 动态规划，先讲例题再练习。"
          :rows="4"
          :disabled="store.creating"
          :error="descriptionError ?? undefined"
          :hint="`${descriptionLength} / ${CLASSROOM_DESCRIPTION_MAX_LENGTH}`"
          data-testid="classroom-description"
        />

        <AppAlert v-if="submitError" tone="danger" data-testid="create-error">
          {{ submitError }}
        </AppAlert>

        <div class="flex flex-wrap items-center gap-3">
          <AppButton type="submit" :loading="store.creating" data-testid="submit-classroom">
            创建课堂
          </AppButton>
          <RouterLink
            :to="{ name: 'teacher-classrooms' }"
            class="inline-flex h-10 items-center justify-center rounded-control border border-border-subtle bg-surface px-4 text-sm font-medium text-ink transition-colors hover:bg-surface-muted"
          >
            返回列表
          </RouterLink>
        </div>
      </form>
    </AppCard>
  </div>
</template>
