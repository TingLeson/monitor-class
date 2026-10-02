import { isApiError } from '@classwatch/api-client'
import { API_ERROR_MESSAGES, apiErrorMessage, type ApiErrorCode } from '@classwatch/shared-types'

/**
 * 课堂相关错误码 → 界面行为（§58；docs/frontend/teacher.md §6）。
 *
 * WHY 集中在一个文件：列表页、新建页、详情页对同一批错误码必须给出同样的说法，
 * 而"冲突要刷新列表"这种规则一旦各写一份，早晚会有一页忘了刷新，
 * 于是界面上停着一个与后端不同的状态。
 */

/**
 * 这些错误码的 message 由后端按场景定制，可能比前端通用文案更有用：
 * - INVALID_REQUEST：具体是哪一条校验不过（名称长度 / 未知字段 / 空账号数组）；
 * - STUDENT_NOT_FOUND / NOT_A_STUDENT / ACCOUNT_DISABLED：后端可能带上更精确的账号。
 *
 * WHY 采纳后端 message 不算"透出原始响应体"：它是错误信封里刻意写给用户看的
 * 一句话（§58 的统一错误结构），不是 HTML / 堆栈 / SQL。真正的原始报文只会被
 * api-client 折叠成 INTERNAL + 通用文案。
 *
 * 判断规则与 admin-web 的 admin-error.ts 保持一致（同样是"只用已经本地化的
 * message"）：后端 message 目前可能是英文，把整句英文渲染在中文界面里比
 * "不够具体的中文"更糟。后端完成本地化后这段判断就可以删掉。
 */
const CODES_WITH_BACKEND_REASON: ReadonlySet<ApiErrorCode> = new Set<ApiErrorCode>([
  'INVALID_REQUEST',
  'STUDENT_NOT_FOUND',
  'NOT_A_STUDENT',
  'ACCOUNT_DISABLED',
])

/** message 里是否含有中日韩文字。 */
function isLocalized(message: string): boolean {
  return /[\u3400-\u9fff\uf900-\ufaff]/.test(message)
}

/**
 * 把任意失败翻译成"可以直接展示给用户"的中文文案。
 *
 * 未登记的（或后端新增而前端还不认识的）错误码一律走通用文案：宁可少说一句，
 * 也不把一段没预期过的内容渲染到界面上。
 */
export function describeClassroomError(error: unknown): string {
  if (!isApiError(error)) return API_ERROR_MESSAGES.INTERNAL

  if (CODES_WITH_BACKEND_REASON.has(error.code)) {
    const message = error.message.trim()
    if (message && message !== API_ERROR_MESSAGES[error.code] && isLocalized(message))
      return message
  }

  return apiErrorMessage(error.code)
}

/**
 * 状态冲突码：请求被拒绝，但拒绝的原因恰好告诉我们"真实状态已经变了"。
 *
 * §7 的状态机只有 CLOSED → OPEN → CLOSED，触发点可能来自另一个标签页或另一台设备，
 * 因此这两个码不是"操作失败"，而是"你看到的界面已经过期"。
 */
export function isClassroomStateConflict(code: ApiErrorCode): boolean {
  return code === 'CLASSROOM_ALREADY_OPEN' || code === 'CLASSROOM_ALREADY_CLOSED'
}

/**
 * 状态冲突时给用户看的那句话。
 *
 * WHY 不直接复用 API_ERROR_MESSAGES（"课堂已经处于开启状态。"）：这条提示必须
 * 同时交代"界面已经刷新"，否则老师会盯着一个刚被刷新、却和自己预期一致的列表
 * 怀疑自己点错了按钮（teacher.md §5 的原文要求：显示真实状态而不是"操作失败"）。
 */
export function classroomConflictNotice(code: ApiErrorCode): string | null {
  switch (code) {
    case 'CLASSROOM_ALREADY_OPEN':
      return '课堂已经是开启状态，列表已刷新。'
    case 'CLASSROOM_ALREADY_CLOSED':
      return '课堂已经是关闭状态，列表已刷新。'
    default:
      return null
  }
}

/**
 * 批量添加里某一行的拒绝原因（§11 / teacher.md §4.2）。
 *
 * 每条都要说清"老师该做什么"：核对拼写 / 确认拿错账号 / 找管理员启用 / 修格式。
 * 只说"添加失败"会让老师面对十个账号无从下手。
 */
export function studentRejectionReason(code: ApiErrorCode): string {
  switch (code) {
    case 'STUDENT_NOT_FOUND':
      return '账号不存在，请核对账号'
    case 'NOT_A_STUDENT':
      return '该账号不是学生账号'
    case 'ACCOUNT_DISABLED':
      return '账号已被停用，请联系管理员启用'
    case 'INVALID_REQUEST':
      return '账号格式不合法'
    default:
      // 后端新增了前端还不认识的拒绝码：给出通用说明，但不编造原因。
      return apiErrorMessage(code)
  }
}

/** 课堂相关表达单里可以被服务端错误命中的字段。 */
export type ClassroomField = 'name' | 'description' | 'accounts'

/**
 * 错误码 → 表单字段。
 *
 * 契约里**没有**为"名称/描述不合法"单独定义错误码，后端统一用 INVALID_REQUEST（400），
 * 而它同时覆盖未知字段、请求体形状错误等原因：前端从 code 上分辨不出是哪一项，
 * 硬挂到名称输入框只会让老师盯着一个没有问题的字段找原因。因此这类错误一律走
 * 页面级提示（并用后端 message 说明细节，见 describeClassroomError）。
 *
 * 保留这个函数而不是让调用方直接写 null：一旦后端补齐字段级错误码，
 * 这里是唯一需要改的地方，三个页面的行为自动跟上。
 */
export function classroomErrorField(code: ApiErrorCode): ClassroomField | null {
  switch (code) {
    /*
     * 整批拒绝的场景（后端对"一个合法账号都没有"的实现）：原因一定出在账号输入上，
     * 挂到那个输入框比顶部提示更有指向性。
     */
    case 'STUDENT_NOT_FOUND':
    case 'NOT_A_STUDENT':
      return 'accounts'
    default:
      return null
  }
}
