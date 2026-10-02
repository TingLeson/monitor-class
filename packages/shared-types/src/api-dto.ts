import type { Uuid } from './common'
import type { ApiErrorCode } from './api-error'

/**
 * 统一错误响应体（§58）。
 *
 * 后端所有非 2xx 都必须返回这个形状；requestId 用于把用户看到的报错与后端
 * 结构化日志（§59 request_id）对起来，排障时是唯一线索，因此不允许省略。
 */
export interface ApiErrorResponse {
  error: {
    code: ApiErrorCode
    message: string
  }
  requestId: string
}

/**
 * Screen Gate 提交的采集诊断信息（§43）。
 *
 * WHY 注释这么长：这不是安全证明。服务端无法真正验证 displaySurface（§19），
 * 前端 `displaySurface === 'monitor'` 才是硬 Gate（§16），这里的数据只用于
 * 诊断与可观测性——例如统计有多少学生"以为"自己共享了整个屏幕。
 * 任何后端逻辑都不得以它为授权依据。
 */
export interface CaptureDiagnostics {
  /** getDisplayMedia 返回的 track settings.displaySurface，正常应为 'monitor'。 */
  displaySurface: string
  width: number
  height: number
}

/**
 * 进入课堂请求（§43）。
 *
 * 前置条件由前端在调用前完成：已通过整个屏幕 Gate。
 * 真正被后端检查的是（§43）：学生已认证、ACTIVE、课堂 OPEN、学生已被授权、
 * 当前存在 ClassroomRun；任何一条不满足都返回对应错误码。
 */
export interface JoinClassroomRequest {
  capture?: CaptureDiagnostics
}

/**
 * 进入课堂响应（§43）。
 *
 * token 是后端用 LiveKit API Secret 签发的短时凭证（§44），
 * Secret 永远不下发到浏览器；前端只持有这个短时 token，且不得持久化到 localStorage。
 */
export interface JoinClassroomResponse {
  sessionId: Uuid
  /** 学生浏览器直连的 LiveKit 地址（wss://...）。 */
  livekitUrl: string
  token: string
}

/**
 * 运行期校验后端错误体。后端升级可能带来前端未知的 code，
 * 此时调用方（api-client）必须降级为 INTERNAL，而不是把未知码当已知码使用。
 */
export function isApiErrorResponse(value: unknown): value is ApiErrorResponse {
  if (typeof value !== 'object' || value === null) return false
  const candidate = value as { error?: unknown; requestId?: unknown }
  if (typeof candidate.requestId !== 'string') return false
  if (typeof candidate.error !== 'object' || candidate.error === null) return false
  const error = candidate.error as { code?: unknown; message?: unknown }
  // 只要求 code 是字符串而不是"已知错误码"：后端升级新增错误码时，前端仍要能读到
  // message 与 requestId（用于展示与排障）；是否降级为 INTERNAL 由 api-client 用
  // isApiErrorCode 判定，避免把一份可读的后端报错误判成"完全无法解析"。
  return typeof error.code === 'string' && typeof error.message === 'string'
}
