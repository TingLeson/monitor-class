import type { IsoDateTime, Uuid } from './common'

/**
 * Business Realtime 的消息契约（§47 / §74，Phase 8 冻结）。
 *
 * §47 明确要求业务消息**不要**塞进 WebRTC DataChannel：Control Plane 必须能在
 * Media Plane 坏掉的时候继续工作。于是有了两条独立通道：
 *
 * ```text
 * /ws/student   ← 学生会话 Cookie 鉴权（同源，浏览器自动带 Cookie）
 * /ws/teacher   ← 老师会话 Cookie 鉴权
 * ```
 *
 * 统一信封（后端冻结，前端逐字实现）：
 *
 * ```json
 * { "type": "SCREEN_LOST", "at": "2026-10-03T10:00:00Z", "data": { ... } }
 * ```
 *
 * 三条纪律：
 *
 * 1. **服务端是权威**（§46）：`SCREEN_LOST` / `SCREEN_RESTORED` 由服务端
 *    （LiveKit webhook）判定；浏览器里的 `track.onended` 只是"快速反馈"。
 *    前端必须能同时处理两条来源且不让它们互相打架（见学生端 media-session store）。
 * 2. **服务端只会推给该收的人**（§26）：学生只会收到"自己被授权的那间课堂"的
 *    `ROOM_OPENED` / `ROOM_CLOSED`，以及**自己**的 `SCREEN_LOST` / `SCREEN_RESTORED`。
 *    因此学生端收到的消息里**不含**其他学生的 id / 姓名 / 在线状态，
 *    客户端也**不得**据此推断其他学生的任何信息（没有字段就无从推断，这正是设计）。
 * 3. **不可信输入**：WebSocket 报文和 HTTP 响应体一样会来自网络，一律先过
 *    {@link isRealtimeEvent} 再使用；解析失败的消息被忽略并计数，绝不抛到 UI。
 */

/**
 * 服务端 → 客户端的业务事件类型全集（§47 的清单，逐字）。
 *
 * Phase 8 只会真的收到前六个；`CAMERA_CHANGED` / `MIC_CHANGED` 属于 Phase 9/10
 * （§24/§25），`PRIVATE_TALK_*` 属于 Phase 10（§31）。提前把类型定下来是为了让
 * 前端在收到它们时**至少不会崩**（走空档分支），而不是为了现在就去实现功能。
 */
export const REALTIME_EVENT_TYPES = [
  'ROOM_OPENED',
  'ROOM_CLOSED',
  'STUDENT_ONLINE',
  'STUDENT_OFFLINE',
  'SCREEN_LOST',
  'SCREEN_RESTORED',
  'CAMERA_CHANGED',
  'MIC_CHANGED',
  'PRIVATE_TALK_REQUEST',
  'PRIVATE_TALK_STARTED',
  'PRIVATE_TALK_ENDED',
] as const

export type RealtimeEventType = (typeof REALTIME_EVENT_TYPES)[number]

/** 客户端心跳报文（冻结契约）：`{"type":"PING"}`。 */
export const REALTIME_PING_TYPE = 'PING' as const
/** 服务端心跳应答（冻结契约）：`{"type":"PONG"}`。它不是业务事件，不进 `onEvent`。 */
export const REALTIME_PONG_TYPE = 'PONG' as const

/* -------------------------------------------------------------------------- */
/* 各事件的 data（逐字对应任务书的表格）                                        */
/* -------------------------------------------------------------------------- */

/** `ROOM_OPENED` → 学生（只推自己被授权的课堂，§48）。 */
export interface RoomOpenedData {
  classroomId: Uuid
  classroomName: string
  runId: Uuid
  openedAt: IsoDateTime
}

/** `ROOM_CLOSED` → 学生 + owner 老师（§49）。 */
export interface RoomClosedData {
  classroomId: Uuid
  runId: Uuid
  closedAt: IsoDateTime
}

/** `STUDENT_ONLINE` → 老师（§12：会话进入 ONLINE）。 */
export interface StudentOnlineData {
  studentId: Uuid
  displayName: string
  /** = StudentSession id = LiveKit identity（§12/§44），老师端订阅用的键。 */
  sessionId: Uuid
}

/**
 * 学生离线的原因（§12 的状态迁移，逐字）。
 *
 * 三种都要能显示出来：`DISCONNECTED`（可能自动恢复）、`LEFT`（人走了）、
 * `ROOM_CLOSED`（课结束了）对老师意味着完全不同的下一步动作。
 */
export const STUDENT_OFFLINE_REASONS = ['DISCONNECTED', 'LEFT', 'ROOM_CLOSED'] as const

export type StudentOfflineReason = (typeof STUDENT_OFFLINE_REASONS)[number]

/** `STUDENT_OFFLINE` → 老师。 */
export interface StudentOfflineData {
  studentId: Uuid
  sessionId: Uuid
  reason: StudentOfflineReason
}

/**
 * 屏幕状态事件的 data：**同一条消息发给两类收件人，载荷不同**（冻结契约）。
 *
 * - 老师收到 `{studentId, sessionId}`：他要在监控墙上定位是哪一张卡片；
 * - 学生本人只收到 `{sessionId}`：§26 的学生间隔离意味着连自己的 studentId
 *   都没有必要下发，更不该出现其他学生的任何字段。
 *
 * 两端都优先用 `sessionId` 匹配（它就是 LiveKit identity，§44），
 * `studentId` 只是老师端名册的另一把键。
 */
export interface TeacherScreenStateData {
  studentId: Uuid
  sessionId: Uuid
}

/** 学生本人收到的屏幕状态（只有自己的会话 id，§26）。 */
export interface StudentScreenStateData {
  sessionId: Uuid
}

export type ScreenStateData = TeacherScreenStateData | StudentScreenStateData

/** `CAMERA_CHANGED` / `MIC_CHANGED` → 老师（Phase 9/10 才会有，§24/§25）。 */
export interface DeviceActiveChangedData {
  studentId: Uuid
  sessionId: Uuid
  active: boolean
}

/**
 * `PRIVATE_TALK_*` → 指定学生（Phase 10，§31）。
 *
 * 本 Phase **只定义类型**：冻结的表格只规定收件人是"指定学生"，没有规定字段
 * （§31 的模型是"老师麦克风只允许目标学生订阅"，业务上更接近媒体层动作）。
 * 因此这里刻意留成"后端字段待定"的空对象形态，守卫对这三个类型只校验信封；
 * 前端一旦真的收到它们也只会走空档分支，不会拿未定义的字段做判断。
 */
export type PrivateTalkData = Record<string, unknown>

/* -------------------------------------------------------------------------- */
/* 事件联合类型                                                                */
/* -------------------------------------------------------------------------- */

/** 事件类型 → data 的映射（`RealtimeEvent<T>` 由它派生）。 */
export interface RealtimeEventDataMap {
  ROOM_OPENED: RoomOpenedData
  ROOM_CLOSED: RoomClosedData
  STUDENT_ONLINE: StudentOnlineData
  STUDENT_OFFLINE: StudentOfflineData
  SCREEN_LOST: ScreenStateData
  SCREEN_RESTORED: ScreenStateData
  CAMERA_CHANGED: DeviceActiveChangedData
  MIC_CHANGED: DeviceActiveChangedData
  PRIVATE_TALK_REQUEST: PrivateTalkData
  PRIVATE_TALK_STARTED: PrivateTalkData
  PRIVATE_TALK_ENDED: PrivateTalkData
}

/**
 * 一条实时事件。`type` 是判别式，因此 `switch (event.type)` 之后
 * `event.data` 会自动收窄到对应形状——这是把契约写进类型系统的全部意义。
 *
 * 泛型参数用于只关心某一类事件的场合（`RealtimeEvent<'ROOM_CLOSED'>`）。
 */
export type RealtimeEvent<T extends RealtimeEventType = RealtimeEventType> = {
  [K in T]: {
    type: K
    /** 服务端产生事件的时间（RFC 3339），展示层才格式化。 */
    at: IsoDateTime
    data: RealtimeEventDataMap[K]
  }
}[T]

/* -------------------------------------------------------------------------- */
/* 运行时守卫                                                                  */
/* -------------------------------------------------------------------------- */

/**
 * 每个事件类型必须存在的 data 字段及其运行时类型。
 *
 * WHY 要逐字段校验而不是"有 type 和 data 就算数"：这是**网络来的**报文。
 * 只校验信封的话，一个 `{"type":"STUDENT_OFFLINE","data":{}}` 会把
 * `undefined` 送进 store，最后表现为监控墙上出现一张名字为空、状态为未定义的卡片——
 * 那正是 §80 说的"用假数据掩盖契约破裂"。宁可丢掉这条消息并计数。
 *
 * 注意 `SCREEN_LOST` / `SCREEN_RESTORED` 只要求 `sessionId`：学生端收到的
 * 就是不含 `studentId` 的版本（§26），把它列成必填会让学生端**必然**丢掉消息。
 */
const REQUIRED_FIELDS: Record<RealtimeEventType, Record<string, 'string' | 'boolean'>> = {
  ROOM_OPENED: {
    classroomId: 'string',
    classroomName: 'string',
    runId: 'string',
    openedAt: 'string',
  },
  ROOM_CLOSED: { classroomId: 'string', runId: 'string', closedAt: 'string' },
  STUDENT_ONLINE: { studentId: 'string', displayName: 'string', sessionId: 'string' },
  STUDENT_OFFLINE: { studentId: 'string', sessionId: 'string', reason: 'string' },
  SCREEN_LOST: { sessionId: 'string' },
  SCREEN_RESTORED: { sessionId: 'string' },
  CAMERA_CHANGED: { studentId: 'string', sessionId: 'string', active: 'boolean' },
  MIC_CHANGED: { studentId: 'string', sessionId: 'string', active: 'boolean' },
  // Phase 10 的载荷字段未冻结：只校验信封，不做字段断言（见 PrivateTalkData）。
  PRIVATE_TALK_REQUEST: {},
  PRIVATE_TALK_STARTED: {},
  PRIVATE_TALK_ENDED: {},
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function isRealtimeEventType(value: unknown): value is RealtimeEventType {
  return typeof value === 'string' && (REALTIME_EVENT_TYPES as readonly string[]).includes(value)
}

export function isStudentOfflineReason(value: unknown): value is StudentOfflineReason {
  return typeof value === 'string' && (STUDENT_OFFLINE_REASONS as readonly string[]).includes(value)
}

/**
 * 把不可信的输入解析成 {@link RealtimeEvent}。
 *
 * 只做"结构是不是这条契约"的判断，不做业务判断（例如不检查这个课堂是不是我的）：
 * 业务判断在 store 里，因为那需要上下文。
 *
 * 返回 false 的输入必须被**忽略并计数**（见 api-client 的 RealtimeSocket）：
 * 让一条畸形报文把整个实时通道打挂，比丢一条消息严重得多（§46 的"客户端只是快速反馈"）。
 */
export function isRealtimeEvent(value: unknown): value is RealtimeEvent {
  if (!isRecord(value)) return false
  if (!isRealtimeEventType(value.type)) return false
  if (typeof value.at !== 'string' || value.at === '') return false
  if (!isRecord(value.data)) return false

  const data = value.data
  for (const [field, kind] of Object.entries(REQUIRED_FIELDS[value.type])) {
    if (typeof data[field] !== kind) return false
  }
  // `reason` 是枚举而不是任意字符串：一个拼错的 reason 会让老师端把它当成
  // "未知离线"显示，而枚举校验能在这里就把它退回去。
  if (value.type === 'STUDENT_OFFLINE' && !isStudentOfflineReason(data.reason)) return false
  return true
}
