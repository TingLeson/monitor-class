import { isApiError } from '@classwatch/api-client'
import { API_ERROR_MESSAGES, apiErrorMessage, type ApiErrorCode } from '@classwatch/shared-types'

/**
 * 管理端错误码 → 界面行为（§58；docs/frontend/admin.md §7）。
 *
 * WHY 集中在一个文件：用户列表、新建账号两个页面要对同一批错误码给出同样的说法
 * （"账号已存在"在新建页挂到账号字段，在列表页则不该出现两种措辞）。
 */

/**
 * 这些错误码的 message 由后端按场景定制，可能比前端通用文案更有用：
 * - INVALID_REQUEST：例如"不能停用自己"、"不能停用最后一个管理员"、PATCH 里出现了
 *   不允许的字段；
 * - PASSWORD_POLICY_VIOLATION：具体是哪条密码规则不满足（长度 / 不得与账号相同）；
 * - ACCOUNT_ALREADY_EXISTS / USER_NOT_FOUND：后端可能带上更精确的账号标识。
 *
 * 为什么这不算"透出原始响应体"：这些 message 是后端错误信封里**刻意写给用户看**的
 * 一句话（§58 的统一错误结构），不是 HTML/堆栈/SQL。真正的原始报文只会被
 * api-client 折叠成 INTERNAL + 通用文案。
 */
const CODES_WITH_BACKEND_REASON: ReadonlySet<ApiErrorCode> = new Set<ApiErrorCode>([
  'INVALID_REQUEST',
  'PASSWORD_POLICY_VIOLATION',
  'ACCOUNT_ALREADY_EXISTS',
  'USER_NOT_FOUND',
])

/**
 * message 里是否含有中日韩文字。
 *
 * WHY 用语言来判断要不要采用后端 message：管理端是纯中文界面，而 Phase 2 的后端
 * message 目前是英文（`internal/apperr` 的默认文案与 service 里的英文句子，例如
 * "the last active administrator cannot be disabled; ..."）。把整句英文渲染在中文
 * 界面里比"不够具体的中文"更糟，因此只采用**已经本地化**的后端 message；
 * 中文兜底文案始终可用，不会出现空白。
 * 后端 message 本地化之后，这个判断就可以删掉。
 */
function isLocalized(message: string): boolean {
  return /[\u3400-\u9fff\uf900-\ufaff]/.test(message)
}

/**
 * 把任意失败翻译成"可以直接显示给用户"的中文文案（要求：错误码 → 中文文案）。
 *
 * 未登记的（或后端新加而前端还不认识的）错误码一律走通用文案：宁可少说一句，
 * 也不把一段没预期过的内容渲染到管理界面里。
 */
export function describeAdminError(error: unknown): string {
  if (!isApiError(error)) return API_ERROR_MESSAGES.INTERNAL

  if (CODES_WITH_BACKEND_REASON.has(error.code)) {
    const message = error.message.trim()
    // 与默认文案相同（后端没定制）就没什么"更具体"可言，直接走映射。
    if (message && message !== API_ERROR_MESSAGES[error.code] && isLocalized(message))
      return message
  }

  return apiErrorMessage(error.code)
}

/** 新建账号表单里可以被服务端错误命中的字段。 */
export type CreateUserField = 'account' | 'password'

/**
 * 错误码 → 表单字段（服务端校验错误要挂到用户正在填的那个输入框上）。
 *
 * WHY 只映射这两个码：契约里只有它们明确属于某个输入框。其余错误——包括
 * `INVALID_REQUEST`（账号格式、不允许的字段等后端统一用它）——一律走页面顶部提示，
 * 因为把一句"请求被拒绝"硬塞进账号框，只会让用户盯着一个没有问题的输入框找原因。
 */
export function createUserErrorField(code: ApiErrorCode): CreateUserField | null {
  switch (code) {
    case 'ACCOUNT_ALREADY_EXISTS':
      return 'account'
    case 'PASSWORD_POLICY_VIOLATION':
      return 'password'
    default:
      return null
  }
}
