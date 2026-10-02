import type { IsoDateTime, Uuid } from './common'

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
 * Classroom = 课程定义（长期存在），Classroom != ClassroomRun（§6）。
 * 老师关闭后再开启同一课堂，会产生新的 ClassroomRun，历史记录因此不会混乱。
 *
 * 状态迁移只能由 owner 老师触发（§48 / §49）：
 * `CLOSED --open--> OPEN --close--> CLOSED`，且每次 CLOSED→OPEN 都必须新建 Run。
 */
export interface Classroom {
  id: Uuid
  name: string
  description: string | null
  /** 必须指向 role == TEACHER 的账号（§10）。开启/关闭/改动的唯一授权依据。 */
  ownerTeacherId: Uuid
  status: ClassroomStatus
  /** 当前进行中的 ClassroomRun；status == CLOSED 时为 null。 */
  currentRunId: Uuid | null
  createdAt: IsoDateTime
  updatedAt: IsoDateTime
}

/**
 * 一次课堂执行（§8）。
 *
 * livekitRoomName 必须是 `lk_<run_uuid>` 这类不透明标识，
 * 禁止使用学生姓名 / 老师姓名 / 真实班级名作为 LiveKit room 或 identity（§8 安全要求）。
 */
export interface ClassroomRun {
  id: Uuid
  classroomId: Uuid
  /** Run 自身的状态与 Classroom 状态同步：OPEN 表示这次执行仍在进行。 */
  status: ClassroomStatus
  livekitRoomName: string
  openedAt: IsoDateTime
  closedAt: IsoDateTime | null
  createdAt: IsoDateTime
}

/**
 * ClassroomStudent = 学生可见性授权（§11）。
 *
 * WHY 是独立实体：学生首页只能返回"被授权的课堂"，后端按本表过滤（§14）；
 * 前端绝不允许先拉全量课堂再自己筛选。
 *
 * 不变量：教师只能向自己拥有的 Classroom 添加学生，且只能选 role == STUDENT
 * 且 status == ACTIVE 的账号（§11、§69）。
 */
export interface ClassroomStudent {
  classroomId: Uuid
  studentId: Uuid
  addedAt: IsoDateTime
  /** 执行添加操作的老师（审计用）。 */
  addedBy: Uuid
}

export function isClassroomStatus(value: unknown): value is ClassroomStatus {
  return typeof value === 'string' && (CLASSROOM_STATUSES as readonly string[]).includes(value)
}
