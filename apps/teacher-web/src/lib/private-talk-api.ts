import type { PrivateTalkResponse, PrivateTalkTarget } from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 老师端私密语音接口（§31 / §76；后端契约已冻结）。
 *
 * 三个动作就是 §31 状态机的全部输入与输出：
 *
 * ```text
 * POST   private-talk {studentId}   IDLE / TALKING(A) → TALKING(B)   （切换 = 再 POST 一次）
 * DELETE private-talk               TALKING(x) → IDLE
 * GET    private-talk               读取当前目标（页面刷新后恢复界面）
 * ```
 *
 * 三条纪律：
 * 1. **目标只有一个，而且由服务端说了算**。本地绝不用 `MonitorStudent` 拼一个
 *    `target` 出来：学生名单可能已经变了，而老师麦克风的订阅权限是服务端按
 *    `sessionId` 设的——本地拼错一个 sessionId，界面会显示"正在与张三讲话"
 *    而声音发给了别人（或没人）。POST 的响应就是答案。
 * 2. **切换不需要先 DELETE**。§31 要求"切换时先撤销旧的"，那是服务端的事
 *    （它会先把旧的撤下来再设新的）；前端多发一次 DELETE 只会在中间留下一个
 *    "没有任何目标"的窗口，而这个窗口里老师的麦克风可能被整个房间听到。
 * 3. **错误码按 §58 分类**：`TEACHER_MIC_REQUIRED` 与 `PRIVATE_TALK_UNAVAILABLE`
 *    是两种完全不同的下一步动作（去开麦 / 换个人），映射在 `./private-talk.ts`。
 */
const BASE_PATH = '/api/v1/teacher/classrooms'

/**
 * 开始（或切换）私密语音目标。
 *
 * `studentId` 是被选中学生的**业务 id**（不是 sessionId）：学生重新进入课堂会拿到
 * 新的 StudentSession，而老师选择的是"这个人"。sessionId 由服务端在响应里给出。
 */
export async function startPrivateTalk(
  classroomId: string,
  studentId: string,
): Promise<PrivateTalkTarget | null> {
  const payload = await api.post<PrivateTalkResponse>(`${BASE_PATH}/${classroomId}/private-talk`, {
    studentId,
  })
  return payload.target
}

/** 结束私密语音（幂等：没有目标时后端也是 204）。 */
export async function stopPrivateTalk(classroomId: string): Promise<void> {
  await api.del<void>(`${BASE_PATH}/${classroomId}/private-talk`)
}

/**
 * 读取当前目标。
 *
 * WHY 页面加载时要调它：私密语音是**服务端**的状态，而老师的界面是本地状态。
 * 刷新、换设备、或者另一个标签页发起过沟通之后，不读一次就会出现"声音正在发出去，
 * 而界面上那个按钮写着「语音沟通」"——老师会以为没在讲，然后对着一个正在广播的
 * 麦克风说下去。
 */
export async function getPrivateTalk(classroomId: string): Promise<PrivateTalkTarget | null> {
  const payload = await api.get<PrivateTalkResponse>(`${BASE_PATH}/${classroomId}/private-talk`)
  return payload.target
}
