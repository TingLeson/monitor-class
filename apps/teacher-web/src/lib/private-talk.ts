import { isApiError } from '@classwatch/api-client'
import { apiErrorMessage, type ApiErrorCode, type MonitorStudent } from '@classwatch/shared-types'

/**
 * 老师端私密语音的界面词表与错误映射（§31 / §58 / §76）。
 *
 * 集中在一个文件里的理由与 `classroom-error.ts` 相同：同一个 `code` 在 Focus 面板、
 * 监督墙页头和将来的任何入口都必须说同一句话；各写一份，早晚会出现
 * "面板说请先开麦、页头说操作失败"这种自相矛盾的界面。
 */

/** 一次私密语音操作的失败（可能是后端错误码，也可能是本地网络/契约失败）。 */
export interface PrivateTalkFailure {
  /** 后端错误码；传输层失败时为 null（那时只有通用文案可用）。 */
  code: ApiErrorCode | null
  /** 可直接展示的中文。 */
  message: string
  /**
   * 是否属于"老师还没开麦"。
   *
   * 单独一个布尔而不是让视图去比字符串：这个码决定了界面上要不要出现
   * **一键开麦**入口（§31 的可执行提示），而那是一个产品行为，不是文案细节。
   */
  micRequired: boolean
}

/**
 * 私密语音失败码的专用文案（§58：客户端按 code 分支）。
 *
 * WHY 不复用 `API_ERROR_MESSAGES` 的默认句子：
 * - `TEACHER_MIC_REQUIRED` 的默认文案已经是"请先开启你的麦克风"，但它出现在
 *   Focus 面板里时还必须配一个**动作**（开麦按钮），这个事实由 `micRequired` 表达；
 * - `PRIVATE_TALK_UNAVAILABLE` 需要一个"下一步"：学生会回来，老师能做的事是等或换人。
 *   默认句子只说"不在课堂中"，老师会反复点同一个按钮。
 */
const PRIVATE_TALK_MESSAGES: Partial<Record<ApiErrorCode, string>> = {
  TEACHER_MIC_REQUIRED: '请先开启你的麦克风，再发起语音沟通。',
  PRIVATE_TALK_UNAVAILABLE: '该学生当前不在课堂中，无法进行语音沟通。他重新进入课堂后可以再试。',
  CLASSROOM_CLOSED: '课堂尚未开启或已经结束，无法进行语音沟通。',
  CLASSROOM_NOT_OWNER: '只有课堂的创建老师可以发起语音沟通。',
  STUDENT_NOT_ASSIGNED: '该学生不在本课堂的名单里，请刷新名单后重试。',
}

/** 把任意失败翻译成"可以直接展示给老师"的中文 + 是否需要开麦。 */
export function toPrivateTalkFailure(cause: unknown): PrivateTalkFailure {
  if (!isApiError(cause)) {
    return { code: null, message: apiErrorMessage('INTERNAL'), micRequired: false }
  }
  const code = cause.code
  return {
    code,
    message: PRIVATE_TALK_MESSAGES[code] ?? apiErrorMessage(code),
    micRequired: code === 'TEACHER_MIC_REQUIRED',
  }
}

/**
 * 当前目标的一句话标签（老师自己的界面记忆，§31）。
 *
 * WHY 必须一直显示它：音频是**看不见的**。老师点完「语音沟通」之后如果界面上
 * 只剩下一个按钮，切到别的卡片、看一会儿别的学生，就再也想不起来自己的麦克风
 * 还在对谁广播——而那时他说的话正被那一个学生听见。
 */
export function describeTalkText(displayName: string): string {
  return `正在与${displayName}语音沟通`
}

/**
 * Focus 面板里那个按钮在**选中状态**下的文案（§30 的 `[语音沟通]` 变成结束入口）。
 *
 * 与页头共用同一句话，只有结尾的动作不同：两处措辞一旦分叉，老师会怀疑
 * "页头说在沟通、按钮说结束"是不是两个不同的东西。
 */
export function describeTalkActionText(displayName: string): string {
  return `${describeTalkText(displayName)} · 结束`
}

/**
 * 这个学生**现在**能不能发起私密语音；不能时给一句可以直接显示的原因。
 *
 * WHY 在界面上就禁用并写出原因，而不是点下去等后端报错：未进入课堂的学生
 * `sessionId === null`，后端必然回 `PRIVATE_TALK_UNAVAILABLE`。让老师点一个
 * 注定失败的按钮，等于把一次产品事实（"他还没进来"）变成一次故障提示。
 *
 * 判据与订阅计划用**同一把尺子**（`isStudentEntered`：有 identity 且会话在线）：
 * 两者一旦分叉，就会出现"按钮可以点、但媒体层没有可说话的会话"这种组合。
 */
export function talkBlockedReason(student: MonitorStudent): string | null {
  if (student.sessionId === null || student.sessionId === '') {
    return '该学生尚未进入课堂，无法发起语音沟通。'
  }
  switch (student.sessionStatus) {
    case 'ONLINE':
    case 'SCREEN_LOST':
      return null
    case 'CONNECTING':
      return '该学生正在进入课堂，稍后即可发起语音沟通。'
    case 'DISCONNECTED':
      return '该学生的连接已断开，暂时无法发起语音沟通。'
    case 'LEFT':
    case 'ROOM_CLOSED':
      return '该学生已经结束了本次课堂，无法发起语音沟通。'
    // 契约保证 sessionStatus 与 sessionId 同时为空或同时有值；真的不变量被破坏时
    // 按"不能发起"处理——宁可少一个入口，也不要发一个注定失败的请求。
    case null:
      return '该学生尚未进入课堂，无法发起语音沟通。'
  }
}

/** 界面是否该让「语音沟通」可点。 */
export function canStartPrivateTalk(student: MonitorStudent): boolean {
  return talkBlockedReason(student) === null
}
