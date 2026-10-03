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
 * Phase 8 起真实收到事件；`CAMERA_CHANGED`（Phase 9，§24）、`MIC_CHANGED` 与
 * `PRIVATE_TALK_*`（Phase 10，§25/§31）都已经有具体载荷，守卫逐字段校验。
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

/* -------------------------------------------------------------------------- */
/* 私密语音（Phase 10，§31/§32）                                               */
/* -------------------------------------------------------------------------- */

/**
 * `PRIVATE_TALK_REQUEST` → **目标学生**（且只在学生麦克风未开启时下发）。
 *
 * 它回答的是"老师想和你通话，请开麦"——因此载荷里只有老师的显示名。
 * §26 的学生间隔离在这里也成立：目标学生拿不到其他学生的任何字段，
 * 也拿不到课堂里"还有谁在听"。
 */
export interface PrivateTalkRequestData {
  teacherDisplayName: string
}

/**
 * `PRIVATE_TALK_STARTED` → 目标学生：仍然是老师的显示名。
 *
 * WHY 不带上 `studentId`：那是学生**自己**的 id，对他来说没有任何用途
 * （匹配会话用 `sessionId`），而下发它只会让"学生端会不会顺手用别人的 id"
 * 变成一个需要回答的问题。没有字段就无从推断（§26 的同一套论证）。
 */
export interface StudentPrivateTalkStartedData {
  teacherDisplayName: string
}

/**
 * `PRIVATE_TALK_STARTED` → **老师**：目标学生的身份与姓名。
 *
 * 老师要在自己的界面上显示"正在和谁讲话"（§31：老师自己必须知道当前目标），
 * 而"谁"只能用 studentId / sessionId / displayName 表达。同一台服务、
 * 同一个事件类型，按收件人给不同形状的载荷——这与 `SCREEN_LOST` 的处理一致。
 */
export interface TeacherPrivateTalkStartedData {
  studentId: Uuid
  sessionId: Uuid
  displayName: string
}

/**
 * `PRIVATE_TALK_STARTED` 的两种收件人形状。
 *
 * 前端按"载荷里有没有 `teacherDisplayName`"分辨自己收到的是哪一版：
 * 学生端只处理学生版，老师端只处理老师版。用联合而不是 `Record<string, unknown>`，
 * 是为了让"老师端拿不到老师姓名"这种契约漂移在编译期就暴露。
 */
export type PrivateTalkStartedData = StudentPrivateTalkStartedData | TeacherPrivateTalkStartedData

/**
 * `PRIVATE_TALK_ENDED` → 目标学生 + 老师：结束的是哪一个会话（§31）。
 *
 * `sessionId` 是两端都有的那一把键（= LiveKit identity，§44），匹配会话必须用它；
 * `studentId` 只在老师端有意义（他要在名册里找到那行）。这与 `SCREEN_LOST`
 * 的两种形状同构：守卫只要求最小公共键，多余字段照收。
 */
export interface PrivateTalkEndedData {
  studentId?: Uuid
  sessionId: Uuid
}

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
  PRIVATE_TALK_REQUEST: PrivateTalkRequestData
  PRIVATE_TALK_STARTED: PrivateTalkStartedData
  PRIVATE_TALK_ENDED: PrivateTalkEndedData
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
 * 每个事件类型可接受的 data 形状（列表 = 允许的**任一**形状）。
 *
 * WHY 要逐字段校验而不是"有 type 和 data 就算数"：这是**网络来的**报文。
 * 只校验信封的话，一个 `{"type":"STUDENT_OFFLINE","data":{}}` 会把
 * `undefined` 送进 store，最后表现为监控墙上出现一张名字为空、状态为未定义的卡片——
 * 那正是 §80 说的"用假数据掩盖契约破裂"。宁可丢掉这条消息并计数。
 *
 * WHY 是"形状列表"而不是单一形状：同一个事件类型对**不同收件人**的载荷不同
 * （§47 的收件人表）。`SCREEN_LOST` 发给老师时带 `studentId`、发给学生本人时
 * 只有 `sessionId`（§26），所以"只要求 sessionId"的那一版必须被接受，
 * 否则学生端会**必然**丢掉每一条屏幕事件。Phase 10 的 `PRIVATE_TALK_STARTED`
 * 是同一回事（学生版只有老师姓名，老师版是目标学生的身份）。
 *
 * 未列出的字段一律不看：契约只承诺这些键存在，向前兼容的多余字段不该让
 * 一条本来可用的消息被丢掉。
 */
const REQUIRED_FIELDS: Record<RealtimeEventType, Record<string, 'string' | 'boolean'>[]> = {
  ROOM_OPENED: [
    { classroomId: 'string', classroomName: 'string', runId: 'string', openedAt: 'string' },
  ],
  ROOM_CLOSED: [{ classroomId: 'string', runId: 'string', closedAt: 'string' }],
  STUDENT_ONLINE: [{ studentId: 'string', displayName: 'string', sessionId: 'string' }],
  STUDENT_OFFLINE: [{ studentId: 'string', sessionId: 'string', reason: 'string' }],
  SCREEN_LOST: [{ sessionId: 'string' }],
  SCREEN_RESTORED: [{ sessionId: 'string' }],
  CAMERA_CHANGED: [{ studentId: 'string', sessionId: 'string', active: 'boolean' }],
  // §25：麦克风的开关只由**老师本人**（owner）收到，载荷与摄像头逐字同形。
  MIC_CHANGED: [{ studentId: 'string', sessionId: 'string', active: 'boolean' }],
  // 学生版：老师是谁（§26：没有别人的任何字段）。
  PRIVATE_TALK_REQUEST: [{ teacherDisplayName: 'string' }],
  PRIVATE_TALK_STARTED: [
    { teacherDisplayName: 'string' },
    { studentId: 'string', sessionId: 'string', displayName: 'string' },
  ],
  // 两端都有的最小公共键是 sessionId；studentId 只对老师端有意义（同 SCREEN_LOST）。
  PRIVATE_TALK_ENDED: [{ sessionId: 'string' }],
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
  const shapes = REQUIRED_FIELDS[value.type]
  const matches = shapes.some((shape) =>
    Object.entries(shape).every(([field, kind]) => typeof data[field] === kind),
  )
  if (!matches) return false
  // `reason` 是枚举而不是任意字符串：一个拼错的 reason 会让老师端把它当成
  // "未知离线"显示，而枚举校验能在这里就把它退回去。
  if (value.type === 'STUDENT_OFFLINE' && !isStudentOfflineReason(data.reason)) return false
  return true
}
