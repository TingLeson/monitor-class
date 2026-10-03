import { ApiError, isApiError } from '@classwatch/api-client'
import type { RealtimeEvent, StudentClassroom } from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { getClassroom, listClassrooms } from '../lib/student-classrooms-api.ts'

/**
 * 学生端课堂状态（§14 / §15 / §26；docs/frontend/student.md §2、§3）。
 *
 * 三条贯穿全文件的原则：
 *
 * 1. **本 Phase 全是只读**。这里没有任何写操作，因此也没有乐观更新可言——
 *    列表与详情都只反映后端最后一次回答。等 Phase 5/6 接入 join 时，
 *    "先本地改状态再等服务端"同样要避免（§15 Step 1 的四项确认只有后端能做）。
 * 2. **列表错误留在 `error`，详情错误留在 `currentError`**。两者要显示在不同的
 *    位置上（列表区域 vs 整页），共用一个字段会让列表的一次失败把详情页也涂红。
 *    两个动作都**不抛异常**：调用方读状态即可，只有需要按错误码分支时才用
 *    `isNotAssigned()` 之类的判断函数。
 * 3. **失败不产生数据**。fetchDetail 失败时清空 `current`（见该函数的说明），
 *    fetchList 失败时**不动** `hasLoaded`——失败的请求没有回答"到底有没有课堂"。
 */
export const useClassroomsStore = defineStore('student-classrooms', () => {
  /* ---------------------------------------------------------------------- */
  /* 我的课堂列表（§14）                                                     */
  /* ---------------------------------------------------------------------- */
  const items = ref<StudentClassroom[]>([])
  /** 列表请求进行中。首次加载显示占位，已有数据时只显示"正在更新"。 */
  const loading = ref(false)
  /** 列表请求失败；保存 ApiError 而不是字符串，文案映射交给视图（错误码是契约）。 */
  const error = ref<ApiError | null>(null)
  /**
   * 是否成功加载过至少一次。
   *
   * WHY 与 `items.length` 分开：加载失败时 items 也是空的，但那时**不能**显示
   * "还没有被加入任何课堂"——那是在用空状态撒谎，而这两件事对学生的下一步动作
   * 完全不同（一个是"去找老师"，一个是"检查网络后重试"）。
   */
  const hasLoaded = ref(false)

  /* ---------------------------------------------------------------------- */
  /* PreJoin 用的课堂详情（§15 Step 2）                                      */
  /* ---------------------------------------------------------------------- */
  const current = ref<StudentClassroom | null>(null)
  const currentLoading = ref(false)
  const currentError = ref<ApiError | null>(null)

  /** 请求序号：后发出的请求结果必须覆盖先发出的（快速切换课堂、重复刷新）。 */
  let listSeq = 0
  let detailSeq = 0
  let listController: AbortController | null = null

  /** 列表为空**且**确实成功加载过，才是真正的空状态。 */
  const isEmpty = computed(() => hasLoaded.value && items.value.length === 0)

  function toApiError(cause: unknown): ApiError {
    return isApiError(cause) ? cause : new ApiError({ code: 'INTERNAL', cause })
  }

  /**
   * 加载我的课堂列表（§14）。
   *
   * 每次刷新都会取消上一次仍在飞行中的请求：列表页的"重试"按钮可能被连点，
   * 不取消就是让多个响应互相竞争（序号已经兜底正确性，这里只是不浪费连接）。
   *
   * 刻意**不**用列表响应去更新 `current`：详情页有自己的请求，两个响应写同一份
   * 状态会让"学生看到哪个课堂"取决于哪个请求先到（网络慢时页面会自己变名字）。
   */
  async function fetchList(): Promise<void> {
    const seq = ++listSeq
    listController?.abort()
    const controller = new AbortController()
    listController = controller

    loading.value = true
    error.value = null
    try {
      const classrooms = await listClassrooms({ signal: controller.signal })
      if (seq !== listSeq) return
      items.value = classrooms
      hasLoaded.value = true
    } catch (cause) {
      if (seq !== listSeq) return
      error.value = toApiError(cause)
      // 刻意不动 hasLoaded / items：失败的请求没有回答"我被安排了哪些课堂"。
    } finally {
      if (seq === listSeq) loading.value = false
    }
  }

  /**
   * 加载课堂详情（§15 Step 1 的接口，Step 2 的页面数据）。
   *
   * 失败时**清空** `current`：否则页面会继续渲染上一个课堂（或本课堂的陈旧数据），
   * 而学生以为自己看的就是刚点进来的那个课堂——"地址栏是 A、卡片是 B"这种错误
   * 在进入课堂前出现是最危险的：学生会带着对错误课堂的预期去共享整块屏幕。
   * 404 的处理也必须建立在这个"没有数据"的干净状态上。
   */
  async function fetchDetail(id: string): Promise<void> {
    const seq = ++detailSeq
    currentLoading.value = true
    currentError.value = null
    try {
      const classroom = await getClassroom(id)
      if (seq !== detailSeq) return
      current.value = classroom
    } catch (cause) {
      if (seq !== detailSeq) return
      current.value = null
      currentError.value = toApiError(cause)
    } finally {
      if (seq === detailSeq) currentLoading.value = false
    }
  }

  /**
   * 实时事件 → 列表与详情（§47 / §48 / §49）。
   *
   * Phase 8 之前，学生要看到"老师开课了"只能靠自己刷新页面；现在服务端在开课/
   * 关课时直接推 `ROOM_OPENED` / `ROOM_CLOSED`，于是列表上的「进入课堂」会自己变可用。
   *
   * 三条边界：
   *
   * 1. **只改事件能证明的那部分**。事件里有 `runId` / `openedAt`（正好是列表卡片
   *    "本次开始于"要显示的 `currentRun`），于是状态与 Run 一起更新；
   *    课堂名之类字段不进事件的处理逻辑——快照仍然是那些字段的权威。
   * 2. **不在列表里的课堂直接忽略**。服务端只会推学生被授权的课堂（§14/§26），
   *    所以出现陌生 id 只可能是竞态（刚被移出名单）。此时**不能凭空造一张卡片**：
   *    那会让一个已经没有权限的学生看到课堂信息。
   * 3. **详情页的 `current` 也要同步**：学生从列表点进 PreJoin 时看到的是详情，
   *    只更新列表会让两处自相矛盾（列表说可进入、详情说未开启）。
   */
  function applyRealtimeEvent(event: RealtimeEvent): void {
    if (event.type === 'ROOM_OPENED') {
      const { classroomId, runId, openedAt } = event.data
      patchClassroom(classroomId, (classroom) => ({
        ...classroom,
        status: 'OPEN',
        currentRun: { id: runId, openedAt },
      }))
      return
    }
    if (event.type === 'ROOM_CLOSED') {
      const { classroomId } = event.data
      patchClassroom(classroomId, (classroom) => ({
        ...classroom,
        status: 'CLOSED',
        currentRun: null,
      }))
    }
  }

  /** 把"课堂是否开着"这一个事实同时写进列表与详情（不在列表里的 id 什么也不做）。 */
  function patchClassroom(
    classroomId: string,
    patch: (classroom: StudentClassroom) => StudentClassroom,
  ): void {
    if (items.value.some((item) => item.id === classroomId)) {
      items.value = items.value.map((item) => (item.id === classroomId ? patch(item) : item))
    }
    if (current.value?.id === classroomId) {
      current.value = patch(current.value)
    }
  }

  /**
   * 清空详情（`:id` 变化、离开页面时调用）。
   *
   * WHY 必须连同在飞行中的响应一起作废：只把 ref 置空的话，上一个课堂的响应
   * 仍可能在之后返回并写进来，页面就出现"地址栏是 B、标题是 A"。
   * 递增序号让那些响应在到达时被丢弃。
   */
  function clearDetail(): void {
    detailSeq += 1
    current.value = null
    currentError.value = null
    currentLoading.value = false
  }

  return {
    items,
    loading,
    error,
    hasLoaded,
    isEmpty,
    current,
    currentLoading,
    currentError,
    fetchList,
    fetchDetail,
    clearDetail,
    applyRealtimeEvent,
  }
})
