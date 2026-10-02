import { isApiError } from '@classwatch/api-client'
import { apiErrorMessage, type ApiErrorCode } from '@classwatch/shared-types'

/**
 * 学生端课堂相关错误码 → 中文文案（§58；docs/frontend/student.md §5）。
 *
 * 两条硬约束：
 *
 * 1. **绝不显示后端原始报文**。§58 明确要求不把 `500 Internal Server Error`
 *    （更不用说 HTML / 堆栈 / SQL）显示给学生。api-client 已经把无法解析的响应体
 *    折叠成 INTERNAL，这里再兜一道：只有本文件里认识的码才给具体文案，
 *    其余一律走通用提示。
 * 2. **不展示后端 message**。与 teacher-web 的 classroom-error.ts 不同，学生端
 *    不采纳后端 message：PreJoin 是学生唯一一次"要不要共享整块屏幕"的决定点，
 *    这里出现一句措辞不可控、可能是英文、可能带上内部细节的话，比一句
 *    不够具体的固定中文更糟。措辞的可控性在学生端比精确性更重要。
 */

/**
 * 本 Phase 会真正遇到的五个码（其余走 API_ERROR_MESSAGES 的通用文案）。
 *
 * WHY 写成显式映射，而不是直接读共享表：「学生在这些情况下看到什么」是一份
 * 需要被逐条审查的清单——学生看不到专业术语、看不到状态码，每句话都要给出
 * 下一步动作（重试 / 重新登录 / 找老师）。共享表是所有入口的默认值，
 * 学生端的关键文案不允许被一次"顺手统一措辞"的改动带跑。
 */
const MESSAGES: Partial<Record<ApiErrorCode, string>> = {
  // 未授权与不存在共用这一个码（§63 最小信息暴露），文案不区分二者。
  STUDENT_NOT_ASSIGNED: '你不在这个课堂的名单里，请联系老师确认。',
  // 会话过期属于正常路径：路由守卫会把学生送回登录页，这里只是兜底的一句话。
  AUTH_REQUIRED: '登录状态已失效，请重新登录。',
  ACCOUNT_DISABLED: '账号已被停用，请联系管理员。',
  // 学生账号无密码（§2.2），限流是唯一的暴力枚举缓解手段；文案不编造等待秒数
  // （后端没有下发 retryAfter，编一个数字等于撒谎），也**不自动重试**。
  RATE_LIMITED: '尝试过于频繁，请稍后再试。',
  NETWORK_ERROR: '网络连接失败，请检查网络后重试。',
}

/**
 * 把任意失败翻译成"可以直接展示给学生"的中文文案。
 *
 * 非 ApiError（理论上不该出现，例如渲染期抛错被 catch 住）与未登记的码
 * 一律降级到共享表的通用文案：宁可少说一句，也不把没预期过的内容渲染出来。
 */
export function describeStudentClassroomError(error: unknown): string {
  if (!isApiError(error)) return apiErrorMessage('INTERNAL')
  return MESSAGES[error.code] ?? apiErrorMessage(error.code)
}

/**
 * 是否是"未被授权进入这个课堂"。
 *
 * 单独抽出来的原因：视图对它的处理不是"显示一条错误"，而是"这一页根本没有内容可
 * 展示"——必须换成说明 + 回列表入口（docs/frontend/student.md §3.4），
 * 停在空白页或给一个重试按钮都在误导学生（重试一百次结果都一样）。
 */
export function isNotAssigned(error: unknown): boolean {
  return isApiError(error) && error.code === 'STUDENT_NOT_ASSIGNED'
}
