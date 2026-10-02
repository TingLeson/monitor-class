import type { ApiErrorCode } from './api-error'
import type { IsoDateTime, Uuid } from './common'
import type { UserStatus } from './user'

/**
 * Classroom 用户可见状态（§7）。
 *
 * WHY 只有两个值：任务书 §7 明确禁止新增 WAITING / STARTING / PAUSED / READY /
 * ENDED / ARCHIVED。课堂要么可进入（OPEN），要么不可进入（CLOSED）。
 * 需要表达"进行到哪一步"时用 ClassroomRun 与 StudentSession 状态，而不是扩展这里。
 */
export const CLASSROOM_STATUSES = ['OPEN', 'CLOSED'] as const

export type ClassroomStatus = (typeof CLASSROOM_STATUSES)[number]

/**
 * 课堂名称长度（§6 / §7）。
 *
 * 去空白后 1–80 字符。前端在输入框里实时校验，只是为了不让用户白填一遍；
 * 真正的拒绝在后端（§63 input validation），前端校验永远不是安全边界。
 */
export const CLASSROOM_NAME_MIN_LENGTH = 1
export const CLASSROOM_NAME_MAX_LENGTH = 80

/** 课堂说明长度上限（§7）：去空白后 ≤ 500 字符，空串视为未填写（description = null）。 */
export const CLASSROOM_DESCRIPTION_MAX_LENGTH = 500

/**
 * 一次批量添加学生的账号数上限（§11 / §42）。
 *
 * 老师手上的凭据是账号，一次粘贴几十个是正常操作，但无限大就等于把请求体大小
 * 变成攻击面；超过上限前端先拦下来，避免发一个注定被拒绝的请求。
 */
export const CLASSROOM_STUDENTS_ADD_MAX = 100

/**
 * Classroom DTO（§6 / §7 / §42 Teacher）。
 *
 * Classroom = 课程定义（长期存在），Classroom != ClassroomRun（§6）：老师关闭后再开启
 * 同一课堂，会产生新的 ClassroomRun，历史记录因此不会混乱。
 *
 * 状态迁移只能由 owner 老师触发（§48 / §49）：
 * `CLOSED --open--> OPEN --close--> CLOSED`，且每次 CLOSED→OPEN 都必须新建 Run。
 *
 * WHY 前端拿到的不是数据表行：列表接口必须带上 `studentCount` 与 `currentRun`，
 * 否则列表页要为每一行再各发一次请求（N+1），或者让前端拿全量名单自己数——
 * 后者等于把"谁在这个课堂里"这件事泄漏到列表页（§14 的精神）。
 */
export interface Classroom {
  id: Uuid
  name: string
  description: string | null
  /** 必须指向 role == TEACHER 的账号（§10）。开启/关闭/改动的唯一授权依据。 */
  ownerTeacherId: Uuid
  status: ClassroomStatus
  /** 有权进入本课堂的学生数（来自 ClassroomStudent，§11）。 */
  studentCount: number
  /**
   * 当前进行中的 ClassroomRun 摘要；status == CLOSED 时为 null。
   *
   * 只带 `id` + `openedAt`：列表页要回答的问题是"这节课开了多久了"，
   * 完整 Run（含 LiveKit room 名）属于服务端内部信息（§8 安全要求）。
   */
  currentRun: ClassroomCurrentRun | null
  createdAt: IsoDateTime
  updatedAt: IsoDateTime
}

/** 列表 / 详情里内嵌的 Run 摘要（§8）。需要完整 Run 时用 open / close 的响应。 */
export interface ClassroomCurrentRun {
  id: Uuid
  openedAt: IsoDateTime
}

/**
 * ClassroomRun（§8）—— 一次课堂执行。
 *
 * 只在 open / close 的响应里下发完整对象：前端需要用它确认"这一次执行开始了/结束了"，
 * 而不是从别处推断。
 *
 * 注意 DTO 里**没有** livekitRoomName：room 名是 `lk_<run_uuid>` 这类不透明标识，
 * 只有真正要进入课堂的学生会通过 join 接口拿到（§43 / §44），
 * 老师端列表不需要它，也就不该下发。
 */
export interface ClassroomRun {
  id: Uuid
  classroomId: Uuid
  /** Run 自身的状态与 Classroom 状态同步：OPEN 表示这次执行仍在进行。 */
  status: ClassroomStatus
  openedAt: IsoDateTime
  closedAt: IsoDateTime | null
}

/**
 * 课堂学生名单里的一行（§11）。
 *
 * WHY 返回的是**账号与显示名**而不是 studentId 列表：老师维护名单时看到的凭据就是账号，
 * 只给一串 uuid 等于让老师对着无法核对的字符串做增删。
 *
 * `id` 是学生账号的 id（DELETE /students/:studentId 用的就是它），不是授权关系 id——
 * ClassroomStudent 的联合主键是 (classroom_id, student_id)（§11）。
 */
export interface ClassroomStudent {
  id: Uuid
  account: string
  displayName: string
  /**
   * 账号状态（§9）。
   *
   * 允许出现 DISABLED：账号被停用后授权关系仍然存在（历史数据保留的立场，§9），
   * 但它无法进入课堂（后端在 join 时拒绝，§43）。界面必须把这个差异显示出来，
   * 否则老师会以为"名单里有他，他就是在上课"。
   */
  status: UserStatus
  addedAt: IsoDateTime
}

/* -------------------------------------------------------------------------- */
/* 响应信封（§42 Teacher / §58）                                                */
/* -------------------------------------------------------------------------- */

/** GET /teacher/classrooms：只包含当前老师拥有的课堂，不分页（docs/frontend/teacher.md §2）。 */
export interface ClassroomListResponse {
  classrooms: Classroom[]
}

/** 详情 / 创建 / 编辑的单对象信封。 */
export interface ClassroomResponse {
  classroom: Classroom
}

/** open / close 的信封：新状态 + 这一次的 Run（§48 / §49）。 */
export interface ClassroomRunResponse {
  classroom: Classroom
  run: ClassroomRun
}

/** GET .../students 的信封。 */
export interface ClassroomStudentListResponse {
  students: ClassroomStudent[]
}

/* -------------------------------------------------------------------------- */
/* 请求体（§7 / §11 / §42）                                                    */
/* -------------------------------------------------------------------------- */

/**
 * 创建课堂（§7）：新建出来的课堂一律是 CLOSED——创建 ≠ 开课，
 * 因此这里**没有** status 字段，也没有 ownerTeacherId（§10：所有权由后端从会话推导，
 * 接受前端传入的所有者等于允许越权指定课堂归属）。
 */
export interface CreateClassroomRequest {
  name: string
  /** 省略或空串表示未填写；后端统一存成 null。 */
  description?: string | null
}

/**
 * 编辑课堂（§7 / §42）。
 *
 * 只接受 name / description：状态只能通过 open / close 改变，未知字段会被后端 400 拒绝
 * 而不是静默忽略——静默忽略会让界面显示"保存成功"却什么都没变。
 * 两个字段都可选，实现"只改一样"的局部更新。
 */
export interface UpdateClassroomRequest {
  name?: string
  description?: string | null
}

/**
 * 批量添加学生（§11）：按账号，而不是"从学生目录里勾选"。
 *
 * WHY 是账号数组：老师手上的凭据就是账号，且 V1 刻意不提供"搜索全部学生"的目录接口
 * （docs/frontend/teacher.md §4.1）——那等于让每个老师都能检索全校学生名单。
 */
export interface AddClassroomStudentsRequest {
  accounts: string[]
}

/* -------------------------------------------------------------------------- */
/* 学生视角（§14 / §15 / §26 / §42 Student；Phase 4）                           */
/* -------------------------------------------------------------------------- */

/**
 * 学生视角的授课老师（§14「王老师」）。
 *
 * WHY 只有 displayName：卡片要回答的问题是"这是谁的课"，而不是"这位老师的账号是什么"。
 * 老师账号属于管理端信息，学生没有任何使用它的场景，下发它只是扩大信息暴露面
 * （§63 最小信息暴露）。前端也**不得**试图用其他字段反推老师身份。
 */
export interface StudentClassroomTeacher {
  displayName: string
}

/**
 * 学生视角的当前 Run 摘要（§8）。
 *
 * 结构上与老师端的 ClassroomCurrentRun 一致，但仍然是**独立的类型**：两者会
 * 各自演进（老师端迟早要 "开了多久 / 在线人数"，学生端只需要"本次开始于几点"），
 * 共用一个类型会让任何一侧的扩展都被迫对另一侧负责。
 */
export interface StudentClassroomCurrentRun {
  id: Uuid
  openedAt: IsoDateTime
}

/**
 * Classroom DTO —— **学生视角**（§14 / §15 / §26 / §42 Student）。
 *
 * WHY 必须与老师端的 `Classroom` 分成两个类型，而不是"少几个字段的同一个类型"：
 * 它们是同一个领域对象面向两类人的两份**不同投影**。老师端的
 * ownerTeacherId / status 之外的 updatedAt 等字段对学生的界面毫无用途，
 * 而一旦共用一个类型，某天为了老师端方便加上的字段就会顺手下发给学生——
 * 那些字段里往往正是 §26 要求隔离的信息。
 *
 * **这里刻意没有 `studentCount`、没有任何名单或其他学生字段。**
 * §26 要求学生之间完全隔离：不展示参与人数、不展示其他学生、不展示其他学生姓名。
 * 因此学生端界面也不得展示"班级人数"之类的信息——DTO 里根本没有这个字段，
 * 前端既没有数据可显示，也不允许用别的方式（例如推断/额外请求）补出来。
 * 在 DTO 层面少一个字段，比事后在每个页面上小心翼翼地藏起来可靠得多：
 * 没有下发，就不存在"某个页面忘了隐藏"这种回归。
 */
export interface StudentClassroom {
  id: Uuid
  name: string
  description: string | null
  /** §7：只有 OPEN / CLOSED。CLOSED 的课堂**照样下发**（§14），界面显示"未开启"。 */
  status: ClassroomStatus
  teacher: StudentClassroomTeacher
  /** status == CLOSED 时为 null（§7 / §8）。 */
  currentRun: StudentClassroomCurrentRun | null
  createdAt: IsoDateTime
}

/**
 * GET /student/classrooms（§14）。
 *
 * 列表**只包含**该学生被 ClassroomStudent 授权的课堂，授权过滤发生在后端 SQL 的
 * JOIN 里。任务书 §14 明确禁止"先给全量课堂、再让前端筛选"：那样未授权的数据
 * 已经进了学生的浏览器，越权已经发生，前端再隐藏也没有意义（§37 授权边界在后端）。
 */
export interface StudentClassroomListResponse {
  classrooms: StudentClassroom[]
}

/**
 * GET /student/classrooms/:id（§15 Step 1 / §42 Student）。
 *
 * 未授权与不存在都返回 404 `STUDENT_NOT_ASSIGNED`，前端无法区分——这是刻意的：
 * 否则学生可以通过试探 id 探测某个课堂是否存在（§63 最小信息暴露）。
 */
export interface StudentClassroomResponse {
  classroom: StudentClassroom
}

/**
 * 批量添加里被拒绝的一项（§11）。
 *
 * 部分成功是刻意的：真实场景里一次粘贴十个账号、其中一两个打错字是常态。
 * 整批失败会让老师反复试错，静默跳过会让老师以为全都加上了。
 */
export interface RejectedClassroomStudent {
  account: string
  /**
   * 逐条原因。契约规定可出现的码：
   * - `STUDENT_NOT_FOUND` 账号不存在（核对拼写）
   * - `NOT_A_STUDENT`     账号是老师 / 管理员（拿错了账号）
   * - `ACCOUNT_DISABLED`  账号已被停用（联系管理员启用）
   * - `INVALID_REQUEST`   账号格式非法（空、过长、含非法字符）
   */
  code: ApiErrorCode
}

/**
 * 批量添加的结果：操作后的**完整名单** + 逐条拒绝原因（§11）。
 *
 * 返回完整名单而不是"新增了哪几个"：前端因此不需要自己把新账号拼进旧列表
 * （那一定会和"本来就在名单里"的幂等语义打架）。
 */
export interface AddClassroomStudentsResponse {
  students: ClassroomStudent[]
  rejected: RejectedClassroomStudent[]
}

export function isClassroomStatus(value: unknown): value is ClassroomStatus {
  return typeof value === 'string' && (CLASSROOM_STATUSES as readonly string[]).includes(value)
}
