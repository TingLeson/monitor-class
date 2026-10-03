import type { Uuid } from './common'

/**
 * 私密语音（§31 / §76，Phase 10 冻结契约）。
 *
 * 后端接口只有三个动作，前端也只允许用这三个：
 *
 * ```text
 * POST   /teacher/classrooms/:id/private-talk   { studentId }  → { target }
 * DELETE /teacher/classrooms/:id/private-talk                  → 204
 * GET    /teacher/classrooms/:id/private-talk                  → { target: {...} | null }
 * ```
 *
 * §31 的状态机是 `IDLE → TALKING(student_id) → IDLE`，**同一时刻只有一个目标**；
 * 因此"当前目标"是**服务端**的状态（老师麦克风的订阅权限在服务端），
 * 前端这份 DTO 只是它的镜像：
 *
 * - 点「语音沟通」= POST，响应里的 `target` 就是权威答案（不要用点击时本地那份
 *   `MonitorStudent` 拼一个出来——学生名单可能已经变了）；
 * - 切到另一个人 = 再 POST 一次（后端会先撤销旧目标再设新的）；
 * - 结束 = DELETE；
 * - 页面刷新/重新进入监督墙 = GET（老师的界面必须能恢复"我正在和谁讲话"，
 *   否则他会对着一个看起来空闲的界面继续讲话）。
 */
export interface PrivateTalkTarget {
  studentId: Uuid
  /** 目标学生姓名（老师界面上"正在与张三语音沟通"用的就是它）。 */
  displayName: string
  /** 目标学生当前的 StudentSession id（= LiveKit identity，§44）。 */
  sessionId: Uuid
}

/**
 * POST / GET 的响应信封。
 *
 * `null` 是合法值而不是"字段缺失"：GET 在"当前没有目标"时必须回 `null`，
 * 前端据此把界面恢复到 IDLE。用 `undefined`/缺字段表达同一件事会让
 * "后端忘了回 target"与"当前确实没有目标"变得无法区分。
 */
export interface PrivateTalkResponse {
  target: PrivateTalkTarget | null
}
