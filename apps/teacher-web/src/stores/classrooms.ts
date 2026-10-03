import { ApiError, isApiError } from '@classwatch/api-client'
import type {
  Classroom,
  ClassroomRunResponse,
  ClassroomStudent,
  CreateClassroomRequest,
  RealtimeEvent,
  RejectedClassroomStudent,
  UpdateClassroomRequest,
} from '@classwatch/shared-types'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  addClassroomStudents as addStudentsRequest,
  closeClassroom as closeClassroomRequest,
  createClassroom as createClassroomRequest,
  getClassroom,
  listClassroomStudents,
  listClassrooms,
  openClassroom as openClassroomRequest,
  removeClassroomStudent,
  updateClassroom as updateClassroomRequest,
} from '../lib/teacher-classrooms-api.ts'

/**
 * 老师端课堂状态（§6 / §7 / §11 / §48 / §49；docs/frontend/teacher.md）。
 *
 * 四条贯穿全文件的原则：
 *
 * 1. **不做乐观更新**（teacher.md §2/§5）。写操作一律先等后端返回：开启/关闭成功时
 *    用返回的 `classroom` DTO 替换那一行（它已经带着新的 `currentRun`），
 *    创建、移除学生这类"结果不由前端决定"的操作直接重新拉取。
 *    本地先改再等服务端，等于让老师看着一个可能被拒绝的状态上课。
 * 2. **列表错误与操作错误分开**：`fetchList` 的失败要显示在列表区域，所以记进 `error`；
 *    `open` / `close` / `create` / `addStudents` 的失败必须由调用方按错误码决定说什么
 *    （冲突要刷新列表、404 要给回列表入口），因此**原样抛出**，不在这里吞掉。
 * 3. **名单不跨课堂存活**（§14 的精神）。`students` 属于"当前打开的那一个课堂"，
 *    切换课堂或离开页面时由 `clearDetail()` 清空——一个残留的名单会让老师
 *    以为某个学生还在新课的名单里，那是隐私与授权双重意义上的错误。
 * 4. **并发只保留最新一次**。列表与详情各有一个请求序号：慢的旧响应不允许覆盖新结果。
 */
export const useClassroomsStore = defineStore('teacher-classrooms', () => {
  /* ---------------------------------------------------------------------- */
  /* 列表（我的课堂，§42 / teacher.md §2，后端按 owner 过滤且不分页）          */
  /* ---------------------------------------------------------------------- */
  const items = ref<Classroom[]>([])
  /** 列表请求进行中。首次加载显示占位，已有数据时只显示"正在更新"。 */
  const loading = ref(false)
  /** 列表请求失败；保存 ApiError 而不是字符串，文案映射交给视图（错误码是契约）。 */
  const error = ref<ApiError | null>(null)
  /**
   * 是否成功加载过至少一次。
   * WHY 与 `items.length` 分开：加载失败时 items 也是空的，但那时**不能**显示
   * "还没有课堂"——那是用空状态撒谎（同 admin-web 的 users store）。
   */
  const hasLoaded = ref(false)

  /* ---------------------------------------------------------------------- */
  /* 详情与名单（§7 / §11 / §42）                                            */
  /* ---------------------------------------------------------------------- */
  const current = ref<Classroom | null>(null)
  const currentLoading = ref(false)
  const currentError = ref<ApiError | null>(null)

  const students = ref<ClassroomStudent[]>([])
  const studentsLoading = ref(false)
  const studentsError = ref<ApiError | null>(null)
  /** 名单是否成功加载过；它决定"还没有学生"这条空状态能不能出现。 */
  const studentsLoaded = ref(false)

  /* ---------------------------------------------------------------------- */
  /* 写操作状态（用于禁用按钮，防止双击产生两次请求，teacher.md §5）           */
  /* ---------------------------------------------------------------------- */
  /** 正在创建课堂（新建页的提交按钮）。 */
  const creating = ref(false)
  /**
   * 正在被写入的课堂 id（开关课堂 / 保存信息 / 添加或移除学生）。
   *
   * WHY 记住是谁而不是一个全局布尔：列表页要**只**禁用那一行的按钮，
   * 其余课堂的操作不该被别人的请求连累。同一时刻只允许一个课堂级写操作，
   * 这是界面主动选择的约束（按钮同时被禁用），不是后端的限制。
   */
  const pendingId = ref<string | null>(null)
  /**
   * 正在进行的是哪一类写操作。
   *
   * WHY 不满足于一个 pendingId：详情页同时摆着"保存信息""开启/关闭""加入名单"三组按钮，
   * 只有知道在等哪一件事，才能让**那一个**按钮转圈、其余按钮只是禁用——
   * 否则移除一个学生的瞬间，"加入名单"也会开始转圈，看起来像正在提交。
   */
  const pendingAction = ref<'open' | 'close' | 'update' | 'students' | 'remove' | null>(null)
  /** 正在被移除的学生 id：只禁用那一行的"移除"，避免重复提交。 */
  const removingStudentId = ref<string | null>(null)

  /**
   * 最近一次批量添加的结果。
   *
   * WHY 留在 store 而不是视图的局部 ref：结果必须和"它属于哪个课堂"一起被清掉，
   * 否则老师切到另一个课堂时还能看到上一个课堂的 rejected 列表。
   */
  const lastAddResult = ref<{
    acceptedCount: number
    rejected: RejectedClassroomStudent[]
  } | null>(null)

  /**
   * 实时事件带来的提示（§47 / §49）。
   *
   * 目前只有一种：**本课堂被关闭**。为什么要有它——老师在详情页看着
   * "已开启"，而课堂其实已经在另一个标签页里被关掉了；只把徽章改成"未开启"
   * 太安静，老师会以为是自己点错了或者页面出了问题。一句说明 + 一个明确的现状
   * 才是诚实。切课堂 / 重新加载时清零。
   */
  const realtimeNotice = ref<string | null>(null)

  /**
   * 已应用的事件数量（§47）。
   *
   * WHY 需要它：快照请求是异步的。一份在事件到达**之前**发出、在事件之后才回来的
   * 响应反映的是更早的时刻——用它覆盖状态，就会把"课堂已经关了"这个刚收到的结论
   * 又改回"已开启"（详情页的徽章与开关按钮会跟着错一次）。因此快照回来时对一下代数，
   * 对不上就丢弃并重取。
   */
  let eventGeneration = 0

  /** 请求序号：后发出的请求结果必须覆盖先发出的（快速切换课堂、重复刷新）。 */
  let listSeq = 0
  let detailSeq = 0
  let studentsSeq = 0
  let listController: AbortController | null = null

  const isEmpty = computed(() => hasLoaded.value && items.value.length === 0)
  /** 列表不分页，总数就是长度（teacher.md §2：一位老师手上的课堂是个位数）。 */
  const total = computed(() => items.value.length)
  /** 正在进行的课堂数（工作台概览用这个数字，而不是再发一个统计接口）。 */
  const openCount = computed(() => items.value.filter((item) => item.status === 'OPEN').length)

  function toApiError(cause: unknown): ApiError {
    return isApiError(cause) ? cause : new ApiError({ code: 'INTERNAL', cause })
  }

  /**
   * 用后端返回的 DTO 替换本地同一个课堂（列表那一行 + 详情头部）。
   *
   * WHY 必须是"整对象替换"而不是逐字段赋值：DTO 是一个整体快照，
   * 逐字段合并会在后端新增字段时悄悄漏掉（例如 currentRun 从 null 变成对象）。
   */
  function applyClassroom(updated: Classroom): void {
    items.value = items.value.map((item) => (item.id === updated.id ? updated : item))
    if (current.value?.id === updated.id) current.value = updated
  }

  /**
   * 用名单的长度同步 `studentCount`。
   *
   * WHY 需要它：移除学生返回 204（没有响应体），而添加学生返回的是"操作后的完整名单"——
   * 两者的长度就是 studentCount 的权威值。为了一次计数再拉一遍列表纯属浪费，
   * 更糟的是那段窗口里界面会显示一个自相矛盾的数字（名单已变、计数未变）。
   */
  function syncStudentCount(classroomId: string, count: number): void {
    const patch = (classroom: Classroom): Classroom =>
      classroom.studentCount === count ? classroom : { ...classroom, studentCount: count }
    items.value = items.value.map((item) => (item.id === classroomId ? patch(item) : item))
    if (current.value?.id === classroomId) current.value = patch(current.value)
  }

  /**
   * 加载我的课堂列表。
   *
   * 每次刷新都会取消上一次仍在飞行中的请求：列表页的"刷新"按钮可能被连点，
   * 不取消就是让多个响应互相竞争（序号已经兜底正确性，这里只是不浪费连接）。
   */
  async function fetchList(): Promise<void> {
    const seq = ++listSeq
    listController?.abort()
    const controller = new AbortController()
    listController = controller

    loading.value = true
    error.value = null
    const generation = eventGeneration
    try {
      const classrooms = await listClassrooms({ signal: controller.signal })
      if (seq !== listSeq) return
      if (generation !== eventGeneration) {
        // 见 eventGeneration 的说明：事件在飞行期间到达，这份快照已经过期。
        void fetchList()
        return
      }
      items.value = classrooms
      hasLoaded.value = true
      // 详情页头部显示的是同一个课堂，列表拿到的更新鲜的状态要同步过去
      // （典型场景：老师在另一个标签页里开了课，然后回到本页刷新列表）。
      const currentId = current.value?.id
      const fresh = currentId ? classrooms.find((item) => item.id === currentId) : undefined
      if (fresh) current.value = fresh
    } catch (cause) {
      if (seq !== listSeq) return
      error.value = toApiError(cause)
      // 刻意不动 hasLoaded：失败的请求没有回答"到底有没有课堂"。
    } finally {
      if (seq === listSeq) loading.value = false
    }
  }

  /**
   * 加载课堂详情。
   *
   * 失败时把 `current` 清空：否则页面会继续渲染上一个课堂（或本课堂的陈旧数据），
   * 而老师以为自己看的是刚点进来的那个课堂——详情页的 404 处理必须建立在一个
   * "没有数据"的干净状态上。
   */
  async function fetchDetail(id: string): Promise<void> {
    const seq = ++detailSeq
    currentLoading.value = true
    currentError.value = null
    try {
      const generation = eventGeneration
      const classroom = await getClassroom(id)
      if (seq !== detailSeq) return
      if (generation !== eventGeneration) {
        // 见 eventGeneration 的说明：事件在飞行期间到达，这份快照已经过期。
        void fetchDetail(id)
        return
      }
      current.value = classroom
      applyClassroom(classroom)
    } catch (cause) {
      if (seq !== detailSeq) return
      current.value = null
      currentError.value = toApiError(cause)
    } finally {
      if (seq === detailSeq) currentLoading.value = false
    }
  }

  /** 加载当前课堂的学生名单（§11）。失败时同样清空，理由与 fetchDetail 相同。 */
  async function fetchStudents(id: string): Promise<void> {
    const seq = ++studentsSeq
    studentsLoading.value = true
    studentsError.value = null
    try {
      const response = await listClassroomStudents(id)
      if (seq !== studentsSeq) return
      students.value = response.students
      studentsLoaded.value = true
      syncStudentCount(id, response.students.length)
    } catch (cause) {
      if (seq !== studentsSeq) return
      students.value = []
      studentsLoaded.value = false
      studentsError.value = toApiError(cause)
    } finally {
      if (seq === studentsSeq) studentsLoading.value = false
    }
  }

  /**
   * 创建课堂（§7：新课堂一律 CLOSED）。
   *
   * 成功后重新拉列表而不是本地插入：列表顺序由后端决定（`created_at DESC, id DESC`），
   * 前端插到哪儿都可能与后端不一致。
   */
  async function create(input: CreateClassroomRequest): Promise<Classroom> {
    creating.value = true
    try {
      const created = await createClassroomRequest(input)
      await fetchList()
      return created
    } finally {
      creating.value = false
    }
  }

  /** 编辑名称 / 描述（§7）。后端返回新 DTO，直接替换那一行与详情头部。 */
  async function update(id: string, input: UpdateClassroomRequest): Promise<Classroom> {
    pendingId.value = id
    pendingAction.value = 'update'
    try {
      const updated = await updateClassroomRequest(id, input)
      applyClassroom(updated)
      return updated
    } finally {
      pendingId.value = null
      pendingAction.value = null
    }
  }

  /**
   * 开启课堂（§48）。
   *
   * 用返回的 DTO 更新那一行：`classroom.currentRun` 是这一次新建的 Run
   * （每次 CLOSED→OPEN 都是新 Run，§8），"本次开始于"因此立刻显示正确的时间。
   * 失败原样抛出：409 CLASSROOM_ALREADY_OPEN 需要调用方刷新列表并说明真实状态。
   */
  async function open(id: string): Promise<ClassroomRunResponse> {
    pendingId.value = id
    pendingAction.value = 'open'
    try {
      const response = await openClassroomRequest(id)
      applyClassroom(response.classroom)
      return response
    } finally {
      pendingId.value = null
      pendingAction.value = null
    }
  }

  /** 关闭课堂（§49）：状态回到 CLOSED、当前 Run 结束、`currentRun` 变回 null。 */
  async function close(id: string): Promise<ClassroomRunResponse> {
    pendingId.value = id
    pendingAction.value = 'close'
    try {
      const response = await closeClassroomRequest(id)
      applyClassroom(response.classroom)
      return response
    } finally {
      pendingId.value = null
      pendingAction.value = null
    }
  }

  /**
   * 批量添加学生（§11，部分成功）。
   *
   * `acceptedCount` = 提交数 - 被拒数：契约规定"已经在本课堂的账号视为成功（幂等）"，
   * 所以被接受的账号数就是老师视角的"成功加入 N 个"；它不依赖本地名单是否加载过
   * （用名单长度差去算，会在名单尚未加载时把"本来就在名单里"的人算成新加入的）。
   */
  async function addStudents(
    id: string,
    accounts: string[],
  ): Promise<{ acceptedCount: number; rejected: RejectedClassroomStudent[] }> {
    pendingId.value = id
    pendingAction.value = 'students'
    lastAddResult.value = null
    try {
      const response = await addStudentsRequest(id, accounts)
      students.value = response.students
      studentsLoaded.value = true
      syncStudentCount(id, response.students.length)
      const result = {
        acceptedCount: Math.max(0, accounts.length - response.rejected.length),
        rejected: response.rejected,
      }
      lastAddResult.value = result
      return result
    } finally {
      pendingId.value = null
      pendingAction.value = null
    }
  }

  /**
   * 移除学生（§11）。
   *
   * 204 没有响应体，所以成功后重新拉名单：让"谁还在名单里"以后端为准，
   * 而不是本地少渲染一行（那正是乐观更新）。
   */
  async function removeStudent(classroomId: string, studentId: string): Promise<void> {
    pendingId.value = classroomId
    pendingAction.value = 'remove'
    removingStudentId.value = studentId
    try {
      await removeClassroomStudent(classroomId, studentId)
      await fetchStudents(classroomId)
    } finally {
      removingStudentId.value = null
      pendingId.value = null
      pendingAction.value = null
    }
  }

  /**
   * 实时事件 → 课堂状态（§47 / §49）。
   *
   * 冻结契约里老师端只收 `ROOM_CLOSED`（`ROOM_OPENED` 只发给学生）。这里只改
   * "课堂是否开着"这一个事实，其余字段继续以 HTTP 快照为准——事件里没有
   * `name` / `studentCount`，凭事件拼一个 DTO 就是造假数据（§80）。
   *
   * 真实场景：老师在标签页 A 关掉了课堂，标签页 B 的详情页 / 列表要跟着变。
   */
  function applyRealtimeEvent(event: RealtimeEvent): void {
    if (event.type !== 'ROOM_CLOSED') return
    const classroomId = event.data.classroomId
    const close = (classroom: Classroom): Classroom => ({
      ...classroom,
      status: 'CLOSED',
      // §49 的第 4 步：课堂关闭后 current_run_id 置空。
      currentRun: null,
    })

    if (current.value?.id === classroomId && current.value.status === 'OPEN') {
      realtimeNotice.value = '本课堂已在其他页面被关闭，学生那边的课堂也已经结束。'
    }
    if (items.value.some((item) => item.id === classroomId)) {
      items.value = items.value.map((item) => (item.id === classroomId ? close(item) : item))
    }
    if (current.value?.id === classroomId) {
      current.value = close(current.value)
    }
    eventGeneration += 1
  }

  /**
   * 实时通道重连成功后的补课（§47）。
   *
   * 断线期间的事件是**永久丢失**的：不重新取快照，老师会一直看着断线前的状态。
   * 只刷新"确实加载过的"东西（列表加载过才刷列表），避免在没有打开过页面时白发请求。
   */
  async function resyncAfterRealtimeReconnect(): Promise<void> {
    if (hasLoaded.value) await fetchList()
    const id = current.value?.id
    if (id !== undefined) await fetchDetail(id)
  }

  /** 清掉实时事件留下的提示（重新加载 / 切换课堂时调用）。 */
  function clearRealtimeNotice(): void {
    realtimeNotice.value = null
  }

  /**
   * 清空详情与名单（切换课堂、离开页面时调用）。
   *
   * WHY 必须连同在飞行中的响应一起作废：只是把 ref 置空的话，上一个课堂的
   * 详情/名单响应仍可能在之后返回并写进来，界面就出现"地址栏是 A、内容是 B"。
   * 递增序号让那些响应在到达时被丢弃。
   */
  function clearDetail(): void {
    detailSeq += 1
    studentsSeq += 1
    current.value = null
    currentError.value = null
    currentLoading.value = false
    students.value = []
    studentsError.value = null
    studentsLoaded.value = false
    studentsLoading.value = false
    lastAddResult.value = null
    realtimeNotice.value = null
  }

  /** 消费掉批量添加的结果提示（老师关闭提示或重新提交时调用）。 */
  function clearAddResult(): void {
    lastAddResult.value = null
  }

  return {
    realtimeNotice,
    items,
    total,
    openCount,
    loading,
    error,
    hasLoaded,
    isEmpty,
    current,
    currentLoading,
    currentError,
    students,
    studentsLoading,
    studentsError,
    studentsLoaded,
    creating,
    pendingId,
    pendingAction,
    removingStudentId,
    lastAddResult,
    fetchList,
    fetchDetail,
    fetchStudents,
    create,
    update,
    open,
    close,
    addStudents,
    removeStudent,
    clearDetail,
    applyRealtimeEvent,
    resyncAfterRealtimeReconnect,
    clearRealtimeNotice,
    clearAddResult,
  }
})
