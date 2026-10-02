import type { MediaTokenResponse, MonitorStudent } from '@classwatch/shared-types'
import type { MonitorResponse } from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 老师端监督接口（§42 Teacher / §51；docs/frontend/teacher.md）。
 *
 * 两个接口回答两个**不同**的问题，前端必须把它们分开使用：
 *
 * - `GET monitor`：业务状态（谁在上课、屏幕是否中断、连接好坏）。这是卡片上
 *   状态徽章的唯一来源（§51）；
 * - `POST media-token`：媒体凭据。它只说明"你可以进这个房间"，**不说明**
 *   现在有谁在房间里，更不说明谁在共享屏幕。
 *
 * 响应信封严格按冻结契约解包（`{students}`）：多一层宽容解析会让"后端少下发
 * 一个字段"在集成阶段变成一句含糊的界面报错，而不是一个明确的契约失败。
 */
const BASE_PATH = '/api/v1/teacher/classrooms'

/**
 * 拉取监督数据（§51）。
 *
 * 每一次调用都会拿到**完整**的学生列表，调用方不做增量合并：增量合并需要
 * "哪些字段是权威的"这份额外的契约，而列表规模不大（一个班几十人），
 * 全量替换反而不会出现"某个学生的状态卡在上一轮"这种幽灵。
 */
export async function getMonitor(
  classroomId: string,
  options: { signal?: AbortSignal } = {},
): Promise<MonitorStudent[]> {
  const payload = await api.get<MonitorResponse>(`${BASE_PATH}/${classroomId}/monitor`, {
    signal: options.signal,
  })
  return payload.students
}

/**
 * 申请老师端媒体 Token（§42 / §44）。
 *
 * 权限由后端在签发时写死（§27：`canSubscribe = true`、`canPublishSources =
 * [microphone]`），前端既不需要、也无从声明自己要什么权限。
 * Token 只放内存，禁止写进 URL / Web Storage / 日志。
 */
export function requestMediaToken(classroomId: string): Promise<MediaTokenResponse> {
  return api.post<MediaTokenResponse>(`${BASE_PATH}/${classroomId}/media-token`)
}
