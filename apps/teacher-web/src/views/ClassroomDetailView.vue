<script setup lang="ts">
import { isApiError } from '@classwatch/api-client'
import {
  CLASSROOM_DESCRIPTION_MAX_LENGTH,
  CLASSROOM_NAME_MAX_LENGTH,
  CLASSROOM_STUDENTS_ADD_MAX,
  type ClassroomStudent,
} from '@classwatch/shared-types'
import {
  AppAlert,
  AppBadge,
  AppButton,
  AppCard,
  AppEmptyState,
  AppModal,
  AppTextArea,
  AppTextField,
  ProtectedRouteGate,
} from '@classwatch/ui'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import {
  classroomConflictNotice,
  classroomErrorField,
  describeClassroomError,
  isClassroomStateConflict,
  studentRejectionReason,
} from '../lib/classroom-error'
import { validateClassroomDescription, validateClassroomName } from '../lib/classroom-form'
import { formatDateTime, formatTimeOfDay } from '../lib/format'
import { describeAddResult, parseStudentAccounts } from '../lib/student-accounts'
import { useClassroomsStore } from '../stores/classrooms'

/**
 * 课堂详情（§7 / §11 / §42 / §48 / §49；docs/frontend/teacher.md §3–§5）。
 *
 * 这一页同时做三件事：改信息、维护学生名单、开关课堂。它们都遵守同一条规则：
 * **先服务端成功，再更新本地**。老师在这里看到的状态必须就是后端的真实状态，
 * 否则他会以为"课已经开了"，而学生那边根本进不来。
 */
const route = useRoute()
const store = useClassroomsStore()

const classroomId = computed(() => {
  const raw = route.params.id
  return typeof raw === 'string' ? raw : ''
})

/* ------------------------------- 编辑信息 -------------------------------- */
const editName = ref('')
const editDescription = ref('')
const editNameError = ref<string | null>(null)
const editDescriptionError = ref<string | null>(null)
const saveError = ref<string | null>(null)
const saveNotice = ref<string | null>(null)
/**
 * 编辑框当前填的是"哪个课堂的数据"。
 * WHY 单独记一份：开关课堂成功后 store.current 会被替换（它带来新的 status / currentRun），
 * 若那时无条件把 current 灌回编辑框，老师刚打了一半的改名内容就被抹掉了。
 */
const formSourceId = ref<string | null>(null)

/* ------------------------------- 开关课堂 -------------------------------- */
const toggleDialogOpen = ref(false)
const toggleError = ref<string | null>(null)
const toggleNotice = ref<string | null>(null)

/* ------------------------------- 学生名单 -------------------------------- */
const accountsInput = ref('')
const addFieldError = ref<string | null>(null)
const addError = ref<string | null>(null)
const removeTarget = ref<ClassroomStudent | null>(null)
const removeError = ref<string | null>(null)
const rosterNotice = ref<string | null>(null)

/** 开关课堂是否正在进行（用于只给这一个按钮转圈）。 */
const toggling = computed(() => store.pendingAction === 'open' || store.pendingAction === 'close')

const editNameLength = computed(() => editName.value.trim().length)
const editDescriptionLength = computed(() => editDescription.value.trim().length)
const parsedAccounts = computed(() => parseStudentAccounts(accountsInput.value))

/** 编辑框里是否有未保存的改动（决定服务端数据能不能覆盖它）。 */
const editDirty = computed(() => {
  const classroom = store.current
  if (!classroom) return false
  return (
    editName.value.trim() !== classroom.name ||
    editDescription.value.trim() !== (classroom.description ?? '')
  )
})

/** 课堂不存在 / 不是自己的：两种都要给回列表的入口，而不是让用户对着空白页。 */
const isMissing = computed(() => {
  const code = store.currentError?.code
  return code === 'CLASSROOM_NOT_FOUND' || code === 'CLASSROOM_NOT_OWNER'
})

function statusLabel(status: 'OPEN' | 'CLOSED'): string {
  return status === 'OPEN' ? '已开启' : '未开启'
}

function resetLocalState(): void {
  editName.value = ''
  editDescription.value = ''
  editNameError.value = null
  editDescriptionError.value = null
  saveError.value = null
  saveNotice.value = null
  formSourceId.value = null
  toggleDialogOpen.value = false
  toggleError.value = null
  toggleNotice.value = null
  accountsInput.value = ''
  addFieldError.value = null
  addError.value = null
  removeTarget.value = null
  removeError.value = null
  rosterNotice.value = null
}

/**
 * 加载一个课堂。
 *
 * 先 `clearDetail()` 再请求：切换课堂时既要把上一个课堂的详情与名单清掉（§14：
 * 名单是某一个课堂的授权信息，绝不能跟着用户走到别的课堂），也要让上一个课堂
 * 还在飞行中的响应作废（store 里用请求序号实现）。
 */
async function load(id: string): Promise<void> {
  store.clearDetail()
  resetLocalState()
  if (!id) return
  await Promise.all([store.fetchDetail(id), store.fetchStudents(id)])
}

/**
 * `:id` 变化时重新拉取（列表页点进来、或直接改地址栏都走这里）。
 * immediate 让首屏加载不依赖 onMounted 的第二次触发。
 */
watch(
  classroomId,
  (id) => {
    void load(id)
  },
  { immediate: true },
)

/**
 * 离开页面时清空详情与名单。
 *
 * WHY 不能只在切换 id 时清：老师从详情页回到列表后，store 里仍留着一份名单，
 * 任何复用这个 store 的页面（例如工作台概览）都可能把它渲染出去。数据不该比
 * 页面活得更久。
 */
onBeforeUnmount(() => {
  store.clearDetail()
})

/** 服务端返回新数据时填充编辑框；有未保存改动时让位于用户。 */
watch(
  () => store.current,
  (classroom) => {
    if (!classroom) return
    if (classroom.id === formSourceId.value && editDirty.value) return
    editName.value = classroom.name
    editDescription.value = classroom.description ?? ''
    formSourceId.value = classroom.id
  },
)

/* ------------------------------- 动作 ----------------------------------- */

function openToggleDialog(): void {
  toggleError.value = null
  toggleNotice.value = null
  toggleDialogOpen.value = true
}

function closeToggleDialog(): void {
  toggleDialogOpen.value = false
  toggleError.value = null
}

function openRemoveDialog(student: ClassroomStudent): void {
  removeError.value = null
  rosterNotice.value = null
  removeTarget.value = student
}

function closeRemoveDialog(): void {
  removeTarget.value = null
  removeError.value = null
}

async function saveEdits(): Promise<void> {
  const id = classroomId.value
  if (!id || store.pendingId !== null) return

  saveError.value = null
  saveNotice.value = null
  editNameError.value = validateClassroomName(editName.value)
  editDescriptionError.value = validateClassroomDescription(editDescription.value)
  if (editNameError.value || editDescriptionError.value) return

  try {
    await store.update(id, {
      name: editName.value.trim(),
      description: editDescription.value.trim() || null,
    })
    saveNotice.value = '课堂信息已保存。'
  } catch (cause) {
    saveError.value = describeClassroomError(cause)
  }
}

async function confirmToggle(): Promise<void> {
  const classroom = store.current
  if (!classroom || store.pendingId !== null) return

  toggleError.value = null
  toggleNotice.value = null
  const opening = classroom.status === 'CLOSED'
  try {
    const response = opening ? await store.open(classroom.id) : await store.close(classroom.id)
    toggleDialogOpen.value = false
    toggleNotice.value = opening
      ? `课堂已开启（本次开始于 ${formatTimeOfDay(response.classroom.currentRun?.openedAt)}），被授权的学生现在可以进入。`
      : '课堂已关闭，本次课堂运行已结束。'
  } catch (cause) {
    const code = isApiError(cause) ? cause.code : null

    if (code !== null && isClassroomStateConflict(code)) {
      // 真实状态已经被别处改过（另一个标签页、另一台设备）：关掉弹窗、按后端重新取
      // 详情与列表，并把"课堂已经是开启/关闭状态"讲清楚（teacher.md §5）。
      toggleDialogOpen.value = false
      toggleNotice.value = classroomConflictNotice(code)
      await Promise.all([store.fetchDetail(classroom.id), store.fetchList()])
      return
    }

    toggleError.value = describeClassroomError(cause)
  }
}

async function submitAddStudents(): Promise<void> {
  const id = classroomId.value
  if (!id || store.pendingId !== null) return

  addFieldError.value = null
  addError.value = null
  rosterNotice.value = null
  store.clearAddResult()

  const { accounts, dropped } = parsedAccounts.value
  if (accounts.length === 0) {
    addFieldError.value = '请输入至少一个学生账号'
    return
  }

  try {
    const result = await store.addStudents(id, accounts)
    /*
     * 被拒的账号回填到输入框，老师改一个就能再提交一次（teacher.md §4.2 的原文期望）。
     * 全部成功时清空输入框——留着刚才那批账号只会让人以为没提交成功。
     */
    accountsInput.value = result.rejected.map((item) => item.account).join('\n')
    if (dropped > 0) {
      rosterNotice.value = `一次最多提交 ${CLASSROOM_STUDENTS_ADD_MAX} 个账号，多出的 ${dropped} 个已在本次忽略。`
    }
  } catch (cause) {
    /*
     * 整批被拒时（例如唯一提交的账号属于老师），原因一定出在账号输入上：
     * 按 classroomErrorField 的映射把它挂到输入框下方，而不是让老师去顶部找一句
     * 与自己刚填的内容对不上的话。
     */
    const code = isApiError(cause) ? cause.code : null
    if (code !== null && classroomErrorField(code) === 'accounts') {
      addFieldError.value = describeClassroomError(cause)
      return
    }
    addError.value = describeClassroomError(cause)
  }
}

async function confirmRemove(): Promise<void> {
  const target = removeTarget.value
  const id = classroomId.value
  if (!target || !id || store.pendingId !== null) return

  removeError.value = null
  rosterNotice.value = null
  try {
    await store.removeStudent(id, target.id)
    removeTarget.value = null
    rosterNotice.value = `已将 ${target.displayName}（${target.account}）移出名单。`
  } catch (cause) {
    if (isApiError(cause) && cause.code === 'STUDENT_NOT_ASSIGNED') {
      // "我以为移除了，其实从来没加进来"是需要被看见的状态差异（teacher.md §4.3）：
      // 说清楚，并把名单刷成后端的样子。
      removeTarget.value = null
      rosterNotice.value = '该学生不在名单里，名单已刷新。'
      await store.fetchStudents(id)
      return
    }
    removeError.value = describeClassroomError(cause)
  }
}
</script>

<template>
  <div class="mx-auto flex max-w-4xl flex-col gap-6">
    <!-- 首次加载：还没有任何数据，用占位而不是渲染一个空课堂。 -->
    <ProtectedRouteGate
      v-if="store.currentLoading && !store.current"
      label="正在加载课堂…"
      hint="如果长时间没有反应，请检查网络连接。"
    />

    <!-- 404（不存在）与 403（不是自己的）都要给回列表的入口，而不是一句干巴巴的报错。 -->
    <AppCard v-else-if="store.currentError && !store.current">
      <AppEmptyState
        tone="danger"
        data-testid="detail-error"
        :title="isMissing ? '无法打开这个课堂' : '课堂加载失败'"
        :description="describeClassroomError(store.currentError)"
      >
        <div class="flex flex-wrap items-center justify-center gap-2">
          <AppButton
            v-if="!isMissing && classroomId"
            variant="secondary"
            size="sm"
            data-testid="detail-retry"
            @click="load(classroomId)"
          >
            重试
          </AppButton>
          <RouterLink
            :to="{ name: 'teacher-classrooms' }"
            class="inline-flex h-8 items-center justify-center rounded-control border border-border-subtle bg-surface px-3 text-xs font-medium text-ink transition-colors hover:bg-surface-muted"
            data-testid="detail-back"
          >
            返回我的课堂
          </RouterLink>
        </div>
      </AppEmptyState>
    </AppCard>

    <template v-else-if="store.current">
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div class="space-y-2">
          <div class="flex flex-wrap items-center gap-2">
            <h1 class="text-2xl font-semibold tracking-tight" data-testid="detail-name">
              {{ store.current.name }}
            </h1>
            <AppBadge
              :label="statusLabel(store.current.status)"
              :tone="store.current.status === 'OPEN' ? 'positive' : 'muted'"
              data-testid="detail-status"
            />
          </div>
          <p class="text-sm text-ink-muted" data-testid="detail-meta">
            学生 {{ store.current.studentCount }} 人
            <template v-if="store.current.status === 'OPEN' && store.current.currentRun">
              · 本次开始于 {{ formatDateTime(store.current.currentRun.openedAt) }}
            </template>
          </p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <RouterLink
            :to="{ name: 'teacher-classroom-monitor', params: { id: store.current.id } }"
            class="inline-flex h-10 items-center justify-center rounded-control border border-border-subtle bg-surface px-4 text-sm font-medium text-ink transition-colors hover:bg-surface-muted"
          >
            进入监督墙
          </RouterLink>
          <AppButton
            :variant="store.current.status === 'OPEN' ? 'danger' : 'primary'"
            :loading="toggling"
            :disabled="store.pendingId !== null"
            data-testid="detail-toggle"
            @click="openToggleDialog"
          >
            {{ store.current.status === 'OPEN' ? '关闭课堂' : '开启课堂' }}
          </AppButton>
        </div>
      </header>

      <!--
        实时事件提示（§47/§49）：课堂在**别的标签页**里被关掉了。
        只把徽章改成"未开启"太安静——老师会以为自己点错了或页面坏了。
        注意这里用的是 store 的字段，因为它必须与详情数据一起被清掉（切换课堂时）。
      -->
      <AppAlert v-if="store.realtimeNotice" tone="info" data-testid="detail-realtime-notice">
        {{ store.realtimeNotice }}
        <template #actions>
          <AppButton variant="ghost" size="sm" @click="store.clearRealtimeNotice()"
            >知道了</AppButton
          >
        </template>
      </AppAlert>

      <AppAlert v-if="toggleNotice" tone="info" data-testid="detail-toggle-notice">
        {{ toggleNotice }}
        <template #actions>
          <AppButton variant="ghost" size="sm" @click="toggleNotice = null">知道了</AppButton>
        </template>
      </AppAlert>

      <!-- 编辑名称 / 描述（§7）：状态只能通过开关课堂改变，所以这里没有状态字段。 -->
      <AppCard title="课堂信息" description="改完点保存。状态不受名称与说明影响。">
        <form class="flex flex-col gap-5" @submit.prevent="saveEdits">
          <AppTextField
            v-model="editName"
            label="课堂名称"
            autocomplete="off"
            :disabled="store.pendingId !== null"
            :error="editNameError ?? undefined"
            :hint="`${editNameLength} / ${CLASSROOM_NAME_MAX_LENGTH}`"
            data-testid="edit-name"
          />
          <AppTextArea
            v-model="editDescription"
            label="课堂说明（可不填）"
            :rows="4"
            :disabled="store.pendingId !== null"
            :error="editDescriptionError ?? undefined"
            :hint="`${editDescriptionLength} / ${CLASSROOM_DESCRIPTION_MAX_LENGTH}`"
            data-testid="edit-description"
          />
          <AppAlert v-if="saveError" tone="danger" data-testid="save-error">{{
            saveError
          }}</AppAlert>
          <AppAlert v-else-if="saveNotice" tone="success" data-testid="save-notice">
            {{ saveNotice }}
          </AppAlert>
          <div>
            <AppButton
              type="submit"
              :loading="store.pendingAction === 'update'"
              :disabled="store.pendingId !== null || !editDirty"
              data-testid="save-classroom"
            >
              保存修改
            </AppButton>
          </div>
        </form>
      </AppCard>

      <!-- 学生名单（§11）：有权限进入这个课堂的学生。 -->
      <AppCard title="学生名单" description="只有名单里的学生可以进入这个课堂。">
        <div class="flex flex-col gap-5">
          <AppAlert v-if="rosterNotice" tone="info" data-testid="roster-notice">
            {{ rosterNotice }}
            <template #actions>
              <AppButton variant="ghost" size="sm" @click="rosterNotice = null">知道了</AppButton>
            </template>
          </AppAlert>

          <ProtectedRouteGate
            v-if="store.studentsLoading && !store.studentsLoaded"
            label="正在加载学生名单…"
          />

          <AppEmptyState
            v-else-if="store.studentsError"
            tone="danger"
            data-testid="students-error"
            title="学生名单加载失败"
            :description="describeClassroomError(store.studentsError)"
          >
            <AppButton variant="secondary" size="sm" @click="store.fetchStudents(classroomId)">
              重试
            </AppButton>
          </AppEmptyState>

          <AppEmptyState
            v-else-if="store.studentsLoaded && store.students.length === 0"
            data-testid="students-empty"
            title="名单里还没有学生"
            description="在下面粘贴学生账号，添加后他们才能进入这个课堂。"
          />

          <ul v-else class="divide-y divide-border-subtle">
            <li
              v-for="student in store.students"
              :key="student.id"
              class="flex flex-wrap items-center justify-between gap-3 py-3"
              :data-testid="`student-row-${student.id}`"
            >
              <div class="min-w-0 space-y-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium text-ink">{{ student.account }}</span>
                  <AppBadge
                    :label="student.status === 'ACTIVE' ? '正常' : '已停用'"
                    :tone="student.status === 'ACTIVE' ? 'positive' : 'attention'"
                  />
                </div>
                <p class="text-sm text-ink-muted">
                  {{ student.displayName }} · 加入于 {{ formatDateTime(student.addedAt) }}
                </p>
                <!-- 停用的账号仍在名单里（授权关系是历史数据，§9），但它进不来。 -->
                <p v-if="student.status === 'DISABLED'" class="text-xs text-status-warning">
                  账号已停用，该学生暂时无法进入课堂。
                </p>
              </div>
              <AppButton
                variant="secondary"
                size="sm"
                :disabled="store.pendingId !== null"
                :loading="store.removingStudentId === student.id"
                :data-testid="`remove-student-${student.id}`"
                @click="openRemoveDialog(student)"
              >
                移除
              </AppButton>
            </li>
          </ul>

          <form
            class="space-y-4 rounded-control border border-border-subtle bg-surface-muted p-4"
            @submit.prevent="submitAddStudents"
          >
            <AppTextArea
              v-model="accountsInput"
              label="批量添加学生"
              placeholder="每行一个账号，也可以直接用逗号或空格分隔，例如：S10086, S10087"
              :rows="4"
              :disabled="store.pendingId !== null"
              :error="addFieldError ?? undefined"
              :hint="`支持换行 / 逗号 / 空格分隔；一次最多 ${CLASSROOM_STUDENTS_ADD_MAX} 个账号`"
              data-testid="add-accounts"
            />

            <AppAlert v-if="addError" tone="danger" data-testid="add-error">{{
              addError
            }}</AppAlert>

            <!--
              部分成功（§11）：被拒的账号逐条说明原因，老师改一个再提交一次。
              这是本页最重要的一条提示——只说"添加失败"会让老师面对十个账号无从下手。
            -->
            <AppAlert
              v-if="store.lastAddResult"
              :tone="store.lastAddResult.rejected.length === 0 ? 'success' : 'info'"
              data-testid="add-result"
            >
              <p class="font-medium">
                {{
                  describeAddResult(
                    store.lastAddResult.acceptedCount,
                    store.lastAddResult.rejected.length,
                  )
                }}
              </p>
              <ul
                v-if="store.lastAddResult.rejected.length > 0"
                class="mt-1 space-y-0.5"
                data-testid="add-rejected"
              >
                <li
                  v-for="rejection in store.lastAddResult.rejected"
                  :key="rejection.account"
                  :data-testid="`rejected-${rejection.account}`"
                >
                  {{ rejection.account }}：{{ studentRejectionReason(rejection.code) }}
                </li>
              </ul>
              <template #actions>
                <AppButton variant="ghost" size="sm" @click="store.clearAddResult()"
                  >知道了</AppButton
                >
              </template>
            </AppAlert>

            <AppButton
              type="submit"
              variant="secondary"
              :loading="store.pendingAction === 'students'"
              :disabled="store.pendingId !== null || !accountsInput.trim()"
              data-testid="submit-add-students"
            >
              加入名单
            </AppButton>
          </form>
        </div>
      </AppCard>
    </template>

    <!-- 开关课堂确认（§48 / §49）：文案必须说清后果，且禁止点遮罩关闭。 -->
    <AppModal
      :open="toggleDialogOpen"
      :tone="store.current?.status === 'OPEN' ? 'danger' : 'default'"
      :title="store.current?.status === 'OPEN' ? '关闭课堂' : '开启课堂'"
      :description="
        store.current?.status === 'OPEN'
          ? '关闭后本次课堂运行结束：正在课堂里的学生会断开连接并回到课堂页。关闭不等于删除，随时可以再次开启。'
          : '开启后，名单里的学生即可进入这个课堂，进入时必须共享整个屏幕，你可以在监督墙看到他们的桌面。'
      "
      :dismissible="false"
      @close="closeToggleDialog"
    >
      <AppAlert v-if="toggleError" tone="danger" data-testid="toggle-dialog-error">
        {{ toggleError }}
      </AppAlert>
      <template #footer>
        <AppButton
          variant="secondary"
          :disabled="store.pendingId !== null"
          @click="closeToggleDialog"
        >
          取消
        </AppButton>
        <AppButton
          :variant="store.current?.status === 'OPEN' ? 'danger' : 'primary'"
          :loading="toggling"
          data-testid="confirm-toggle"
          @click="confirmToggle"
        >
          {{ store.current?.status === 'OPEN' ? '确认关闭' : '确认开启' }}
        </AppButton>
      </template>
    </AppModal>

    <!-- 移除确认：移除只影响"能不能进入课堂"，不删除账号，也不影响历史记录（§9）。 -->
    <AppModal
      :open="removeTarget !== null"
      tone="danger"
      :title="`将 ${removeTarget?.displayName ?? ''} 移出名单`"
      :description="`移除后 ${removeTarget?.account ?? ''} 将无法进入这个课堂。账号本身不会被删除，随时可以重新加入。`"
      :dismissible="false"
      @close="closeRemoveDialog"
    >
      <AppAlert v-if="removeError" tone="danger" data-testid="remove-dialog-error">
        {{ removeError }}
      </AppAlert>
      <template #footer>
        <AppButton
          variant="secondary"
          :disabled="store.pendingId !== null"
          @click="closeRemoveDialog"
        >
          取消
        </AppButton>
        <AppButton
          variant="danger"
          :loading="store.removingStudentId !== null"
          data-testid="confirm-remove"
          @click="confirmRemove"
        >
          确认移除
        </AppButton>
      </template>
    </AppModal>
  </div>
</template>
