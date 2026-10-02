import type {
  AddClassroomStudentsRequest,
  AddClassroomStudentsResponse,
  Classroom,
  ClassroomListResponse,
  ClassroomResponse,
  ClassroomRunResponse,
  ClassroomStudentListResponse,
  CreateClassroomRequest,
  UpdateClassroomRequest,
} from '@classwatch/shared-types'
import { api } from './api.ts'

/**
 * 老师端课堂管理接口（§42 Teacher / §48 / §49；docs/frontend/teacher.md）。
 *
 * 复用本 app 的 api client（./api.ts）：Cookie 会话、写请求自动附加 X-CSRF-Token、
 * 统一错误体 → ApiError 都由它保证。这里只负责"路径 + 请求体 + 响应信封形状"，
 * 不做任何业务判断——所有权（§37）、状态机（§7）、账号资格（§11）的唯一执行点都是后端。
 *
 * 响应信封严格按冻结契约解包（列表 `{classrooms}`；其余 `{classroom}`；
 * open/close 额外带 `{run}`）：多一层宽容解析会让"后端少下发一个字段"这种事
 * 在集成阶段变成界面上一句含糊的报错，而不是一个明确的契约失败。
 */
const BASE_PATH = '/api/v1/teacher/classrooms'

/**
 * 列出**自己拥有**的课堂（§14 / §42）。
 *
 * 后端按 owner_teacher_id 过滤，前端不做筛选、也无从筛选：别人的课堂根本不会下发。
 * 不分页（docs/frontend/teacher.md §2）：一个老师手上的课堂是个位数。
 * `signal` 透传给 fetch，页面卸载时可以取消仍在飞行中的请求。
 */
export async function listClassrooms(options: { signal?: AbortSignal } = {}): Promise<Classroom[]> {
  const payload = await api.get<ClassroomListResponse>(BASE_PATH, { signal: options.signal })
  return payload.classrooms
}

/**
 * 创建课堂（§7）：新课堂一律是 CLOSED，创建 ≠ 开课。
 *
 * 请求体里**没有** ownerTeacherId（§10）：所有权由后端从会话推导，
 * 接受前端传入的所有者等于允许越权指定课堂归属。
 */
export async function createClassroom(input: CreateClassroomRequest): Promise<Classroom> {
  const payload = await api.post<ClassroomResponse>(BASE_PATH, input)
  return payload.classroom
}

/** 课堂详情（§42）。不是自己的课堂由后端返回 403 CLASSROOM_NOT_OWNER。 */
export async function getClassroom(id: string): Promise<Classroom> {
  const payload = await api.get<ClassroomResponse>(`${BASE_PATH}/${id}`)
  return payload.classroom
}

/** 编辑名称 / 描述（§7）。只允许这两个字段；状态只能通过 open / close 改变。 */
export async function updateClassroom(
  id: string,
  input: UpdateClassroomRequest,
): Promise<Classroom> {
  const payload = await api.patch<ClassroomResponse>(`${BASE_PATH}/${id}`, input)
  return payload.classroom
}

/** 课堂学生名单（§11）。老师只能读自己课堂的名单，越权由后端判定。 */
export async function listClassroomStudents(id: string): Promise<ClassroomStudentListResponse> {
  return api.get<ClassroomStudentListResponse>(`${BASE_PATH}/${id}/students`)
}

/**
 * 按账号批量添加学生（§11）。
 *
 * 返回的是**部分成功**的结果：合法账号照常加入，不合法的逐条给出 code，
 * 调用方必须把 rejected 展示出来（整批失败与静默跳过都不允许，见 teacher.md §4.2）。
 *
 * `accounts` 为空数组时后端返回 400 INVALID_REQUEST，因此调用方要先在本地拦下来。
 */
export function addClassroomStudents(
  id: string,
  accounts: string[],
): Promise<AddClassroomStudentsResponse> {
  const body: AddClassroomStudentsRequest = { accounts }
  return api.post<AddClassroomStudentsResponse>(`${BASE_PATH}/${id}/students`, body)
}

/**
 * 移除学生（§11）。
 *
 * 成功是 204（无响应体），因此返回 void；学生本来就不在名单里时后端返回
 * 404 STUDENT_NOT_ASSIGNED——"我以为移除了，其实从来没加进来"是需要被看见的差异，
 * 所以这里**不**把 404 当成成功吞掉。
 */
export async function removeClassroomStudent(id: string, studentId: string): Promise<void> {
  await api.del<void>(`${BASE_PATH}/${id}/students/${studentId}`)
}

/**
 * 开启课堂（§48）。
 *
 * 后端在一个事务里完成：校验所有权 → 校验当前是 CLOSED → 新建 ClassroomRun →
 * 生成不透明 LiveKit room 名 → 置 OPEN 并写 current_run_id。因此每次成功调用都
 * 对应一个**新的** Run（§8），关闭后再开启不会复用旧 Run。
 */
export function openClassroom(id: string): Promise<ClassroomRunResponse> {
  return api.post<ClassroomRunResponse>(`${BASE_PATH}/${id}/open`)
}

/** 关闭课堂（§49）：结束当前 Run、清空 current_run_id，并由后端统一收尾在线会话。 */
export function closeClassroom(id: string): Promise<ClassroomRunResponse> {
  return api.post<ClassroomRunResponse>(`${BASE_PATH}/${id}/close`)
}
