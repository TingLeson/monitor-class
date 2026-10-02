import type {
  CaptureDiagnostics,
  JoinClassroomRequest,
  JoinClassroomResponse,
} from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 学生端"进入/离开课堂"接口（§42 Student / §43 / §44；docs/frontend/student.md §3.3）。
 *
 * 与 `student-classrooms-api.ts` 分开的原因：那两个是**只读**的课堂门户接口，
 * 这两个是会真的产生副作用（创建 StudentSession、签发媒体 Token）的媒体入口。
 * 混在一个文件里，会让"学生端哪些调用会产生服务端状态"这件事在读代码时消失。
 *
 * 前端在这里**不做任何授权判断**（§37）：是否被安排、课堂是否开启、账号是否可用
 * 全部由后端判定；前端只负责把 §43 的诊断字段原样提交，并把返回的
 * `{sessionId, livekitUrl, token}` 交给内存 store。
 */

const CLASSROOMS_PATH = '/api/v1/student/classrooms'
const SESSIONS_PATH = '/api/v1/student/sessions'

/**
 * 进入课堂（§43）。
 *
 * 调用前置条件（顺序是设计，不能变，§18）：学生已经通过整屏 Gate，
 * 手上有一条活着的 `displaySurface === 'monitor'` 轨道。因此调用方必须把
 * Gate 的诊断信息传进来——它**不是**安全证明（§19），只是可观测性数据：
 * 用来在排障时回答"这位学生当时以为自己共享了什么"。
 *
 * 失败语义（冻结契约）：404 STUDENT_NOT_ASSIGNED / 409 CLASSROOM_CLOSED /
 * 401 / 403。调用方按 code 分支决定"还能不能重试"（见 PreJoin 视图）。
 */
export async function joinClassroom(
  classroomId: string,
  capture: CaptureDiagnostics,
): Promise<JoinClassroomResponse> {
  const body: JoinClassroomRequest = { capture }
  return api.post<JoinClassroomResponse>(`${CLASSROOMS_PATH}/${classroomId}/join`, body)
}

/**
 * 离开课堂（§42 Student）。
 *
 * 成功是 204（无响应体），因此返回 void。
 *
 * WHY 这个调用必须是"尽力而为、失败不阻塞退出"：学生点「离开课堂」时，页面必须
 * 立刻断开媒体并回列表——把 UI 卡在一个可能超时的 POST 上，等于强迫学生继续被
 * 共享着屏幕等网络超时。调用方（store）会吞掉这里的异常并照常完成本地退出；
 * 服务端还有 LiveKit 的 participant_left / track_unpublished 兜底（§45/§46），
 * 因此"少一次 leave 上报"不会让后端永远认为这个学生在线。
 */
export async function leaveSession(sessionId: string): Promise<void> {
  await api.post<void>(`${SESSIONS_PATH}/${sessionId}/leave`)
}
