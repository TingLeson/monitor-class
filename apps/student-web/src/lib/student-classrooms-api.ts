import type {
  StudentClassroom,
  StudentClassroomListResponse,
  StudentClassroomResponse,
} from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 学生端课堂接口（§42 Student / §14 / §15；docs/frontend/student.md §2、§3）。
 *
 * 复用本 app 的 api client（./api.ts）：Cookie 会话、统一错误体 → ApiError 都由它保证。
 * 这里只负责"路径 + 响应信封形状"，不做任何业务判断——学生是否被授权、课堂是否
 * 开启（§15 Step 1 的四项确认）**全部由后端判定**，前端拿到的就已经是"允许他知道的
 * 那一部分"（§37：授权边界在后端）。
 *
 * Phase 4 只有这两个只读接口。join / media-token / leave / events 属于 Phase 5/6，
 * 本文件**刻意不预置**它们：一个还没实现的接口封装很容易被下一个 Phase 顺手调用，
 * 而它背后此刻根本没有后端实现。
 *
 * 响应信封严格按冻结契约解包（列表 `{classrooms}`、详情 `{classroom}`）：
 * 多一层宽容解析会让"后端少下发一个字段"在集成阶段变成界面上一句含糊的报错，
 * 而不是一个明确的契约失败。
 */
const BASE_PATH = '/api/v1/student/classrooms'

/**
 * 列出**自己被授权进入**的课堂（§14）。
 *
 * 后端在 SQL JOIN 里完成授权过滤（`WHERE cs.student_id = $1`），前端既不筛选、
 * 也无从筛选：别人的课堂根本不会下发。任务书 §14 明确禁止"GET /all-classrooms
 * 再由前端过滤"——那样未授权的数据已经进了学生的浏览器，越权已经发生。
 *
 * 排序同样由后端决定（已开启的在前），前端**不重新排序**：前端再排一遍只会
 * 掩盖一次排序回归，而且列表规模很小，不值得为此引入一份本地排序规则。
 *
 * `signal` 透传给 fetch：离开页面时可以取消仍在飞行中的请求。
 */
export async function listClassrooms(
  options: { signal?: AbortSignal } = {},
): Promise<StudentClassroom[]> {
  const payload = await api.get<StudentClassroomListResponse>(BASE_PATH, { signal: options.signal })
  return payload.classrooms
}

/**
 * 课堂详情（§15 Step 1 → Step 2 之间）。
 *
 * 未授权与不存在都返回 404 `STUDENT_NOT_ASSIGNED`，调用方**不得**尝试区分二者
 * （§63 最小信息暴露：学生不能通过试探 id 探测某个课堂是否存在），
 * 界面只需给出同一句说明 + 回列表入口。
 */
export async function getClassroom(id: string): Promise<StudentClassroom> {
  const payload = await api.get<StudentClassroomResponse>(`${BASE_PATH}/${id}`)
  return payload.classroom
}
