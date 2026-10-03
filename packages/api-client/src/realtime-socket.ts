import {
  REALTIME_PING_TYPE,
  REALTIME_PONG_TYPE,
  isRealtimeEvent,
  type RealtimeEvent,
} from '@classwatch/shared-types'

/**
 * Business Realtime 的**传输层**客户端（§47 / §52 / §74）。
 *
 * WHY 放在 `@classwatch/api-client` 而不是各 app 自己写一份：
 * 这个类里没有一行业务逻辑（它不知道"课堂"是什么），它只负责"把一条 WebSocket
 * 维持在可用状态并把报文解析成事件"。学生端与老师端对这件事的要求逐字相同
 * （同样的重连、同样的心跳、同样的"不可信输入"处理），写在两个 app 里迟早会漂移成
 * "一边有心跳一边没有"。**业务路由**（哪个事件交给哪个 store）仍然留在各 app ——
 * 共享包不该知道任何 app 的 store 长什么样，这跟 HTTP 客户端不实现登录接口是同一个理由。
 *
 * 三条纪律：
 *
 * 1. **URL 与日志里不许出现凭据**。鉴权走同源 Cookie（§38/§41），浏览器自动带；
 *    因此地址就是 `/ws/student` 这类路径，没有 token 可放、也没有 token 会漏。
 *    本类**不打印 URL**，只在状态迁移时打一句不带值的说明。
 * 2. **网络来的报文一律不可信**：过 {@link isRealtimeEvent}，失败就丢弃并计数，
 *    绝不抛到 UI（§80 的"不要用假数据掩盖契约破裂"）。
 * 3. **断线是可恢复的常态**：指数退避 + 抖动重连，心跳超时判定假死，
 *    并且把 `connecting | open | closed` 明确暴露给 UI——界面必须能说出
 *    "现在不是最新状态"，而不是假装一切正常。
 */

/** 实时通道的连接状态（冻结的 UI 词表，只有这三个值）。 */
export const REALTIME_CONNECTION_STATES = ['connecting', 'open', 'closed'] as const

export type RealtimeConnectionState = (typeof REALTIME_CONNECTION_STATES)[number]

/**
 * 浏览器 `WebSocket` 里我们真正会用到的那一小部分。
 *
 * WHY 抽成接口：测试要注入假 socket（`happy-dom` 里没有真实网络，也不该有），
 * 而注入点必须是"结构相同"的，不能是 `any`（§80：不用 any）。
 * 用 `onopen/onmessage/onclose/onerror` 四个属性而不是 `addEventListener`：
 * 它们与原生 `WebSocket` 完全一致，实现最薄，也最容易在假 socket 里复刻；
 * 参数类型直接用 DOM 的事件类型，这样原生 `WebSocket` 天然就是本接口的一个实现，
 * 不需要任何类型断言。
 */
export interface RealtimeSocketLike {
  onopen: ((event: Event) => void) | null
  onmessage: ((event: MessageEvent) => void) | null
  onclose: ((event: CloseEvent) => void) | null
  onerror: ((event: Event) => void) | null
  send(data: string): void
  close(code?: number, reason?: string): void
}

export type RealtimeSocketFactory = (url: string) => RealtimeSocketLike

/** 只打印，不上报：通道出问题时的第一现场是浏览器控制台，不是后端日志（§59）。 */
export interface RealtimeLogger {
  warn(message: string): void
}

export interface RealtimeSocketOptions {
  /** 同源地址，例如 `ws://localhost:5173/ws/student`；**禁止**携带凭据。 */
  url: string
  /** 每一条通过校验的事件（`PONG` 由本类内部消化，不会出现在这里）。 */
  onEvent(event: RealtimeEvent): void
  /** 状态迁移回调；与 {@link RealtimeSocket.state} 同步触发。 */
  onStateChange?(state: RealtimeConnectionState): void
  /** 测试注入点；不传则用浏览器原生 `WebSocket`。 */
  socketFactory?: RealtimeSocketFactory
  /** 心跳间隔：定期发 `{"type":"PING"}`。 */
  heartbeatIntervalMs?: number
  /** PING 发出后多久没收到任何回包就判定通道假死。 */
  heartbeatTimeoutMs?: number
  /** 退避基数：第 n 次重连等待 base × 2^n（上限 reconnectMaxMs）。 */
  reconnectBaseMs?: number
  /** 退避上限：30 秒。再长就不如让用户手动刷新了。 */
  reconnectMaxMs?: number
  /** 抖动比例（0–1）：避免整个班的学生在同一毫秒一起重连。 */
  reconnectJitterRatio?: number
  /**
   * 连续多少次"连开都没开就断开"之后放弃重连（判定为握手被拒）。
   *
   * WHY 仍然保留这个兜底：服务端在**升级成功**时用 close code 4401/4403 明确拒绝
   * （见 AUTH_CLOSE_CODES，那条路径是确定的，不会重连）。但如果请求在到达应用之前
   * 就被代理/网关拒掉（握手没完成、或部署的是不带 close code 的旧版本），
   * 浏览器就只剩"没打开过"这一个信号——那时用次数止损比无限重试诚实。
   */
  handshakeFailureLimit?: number
  /** 抖动随机源（测试注入确定性值）。 */
  random?: () => number
  logger?: RealtimeLogger
}

/**
 * 服务端用于拒绝实时连接的 close code（与 services/api/internal/realtime 的常量一致）。
 *
 * 4401：没有有效会话（未登录 / 会话过期 / 账号被停用）
 * 4403：会话有效但属于另一个入口（例如拿着学生会话连 /ws/teacher）
 *
 * 应用自定义区间是 4000–4999，不会与协议保留码冲突。
 */
export const REALTIME_AUTH_CLOSE_CODES = [4401, 4403] as const
const AUTH_CLOSE_CODES: readonly number[] = REALTIME_AUTH_CLOSE_CODES

const DEFAULTS = {
  heartbeatIntervalMs: 25_000,
  heartbeatTimeoutMs: 10_000,
  reconnectBaseMs: 1_000,
  reconnectMaxMs: 30_000,
  reconnectJitterRatio: 0.2,
  handshakeFailureLimit: 3,
} as const

/** 丢弃报文时打的日志最多这么多条，避免一个坏掉的服务端把控制台刷爆。 */
const DROP_LOG_LIMIT = 3

function defaultSocketFactory(url: string): RealtimeSocketLike {
  if (typeof WebSocket === 'undefined') {
    throw new Error('realtime: 当前环境没有 WebSocket 实现')
  }
  return new WebSocket(url)
}

export class RealtimeSocket {
  readonly #options: RealtimeSocketOptions
  readonly #logger: RealtimeLogger

  #state: RealtimeConnectionState = 'closed'
  #socket: RealtimeSocketLike | null = null
  /** 当前这一次尝试是否成功打开过（用来区分"握手被拒"与"连上又断了"）。 */
  #attemptOpened = false
  #attempt = 0
  #handshakeFailures = 0
  #authFailed = false
  #disposed = false
  #awaitingPong = false
  #dropped = 0
  #reconnectTimer: ReturnType<typeof setTimeout> | null = null
  #heartbeatTimer: ReturnType<typeof setInterval> | null = null
  #pongTimer: ReturnType<typeof setTimeout> | null = null

  constructor(options: RealtimeSocketOptions) {
    this.#options = options
    this.#logger = options.logger ?? console
  }

  get state(): RealtimeConnectionState {
    return this.#state
  }

  /**
   * 是否已判定"握手被拒"（最可能就是 401 会话失效）。
   *
   * 判定是**启发式**的（见 handshakeFailureLimit）：浏览器拿不到状态码，
   * 前端能做的只是"连开都开不起来、连续几次之后不再无脑重试"。
   * UI 因此不能说"你被登出了"，只能说"实时通道没建立起来，可能需要重新登录"。
   */
  get authFailed(): boolean {
    return this.#authFailed
  }

  /** 被丢弃的报文数（解析失败 / 未知类型）。只用于排障，不上报、不弹窗。 */
  get droppedMessages(): number {
    return this.#dropped
  }

  /** 幂等：已经连上或正在连时不重复建连。 */
  connect(): void {
    if (this.#disposed || this.#socket !== null) return
    this.#attemptOpened = false
    let socket: RealtimeSocketLike
    try {
      socket = (this.#options.socketFactory ?? defaultSocketFactory)(this.#options.url)
    } catch (cause) {
      // 环境不支持 WebSocket / URL 非法：当作一次失败的尝试，走正常的退避路径，
      // 而不是把异常抛给调用方（调用方是 store，它没有任何补救手段）。
      this.#logger.warn(`realtime: 无法创建连接（${nameOf(cause)}）`)
      this.#handleAttemptFailed()
      return
    }
    this.#socket = socket
    socket.onopen = () => {
      this.#attemptOpened = true
      this.#attempt = 0
      this.#handshakeFailures = 0
      this.#setState('open')
      this.#startHeartbeat()
    }
    socket.onmessage = (event) => {
      this.#handleMessage(event.data)
    }
    // onerror 之后浏览器一定会再触发 onclose；重连只在 onclose 里调度一次，
    // 否则一次断线会被处理两遍（两次退避、两条连接）。
    socket.onerror = () => undefined
    socket.onclose = (event: CloseEvent) => {
      this.#handleClose(event.code)
    }
    this.#setState('connecting')
  }

  /**
   * 永久关闭：清定时器、摘回调、不再重连。
   *
   * WHY 必须摘回调再 close：真实浏览器的 `close()` 会异步触发 `onclose`，
   * 留着回调就会走一遍"断线 → 调度重连"的逻辑，把已经决定关闭的通道又拉起来。
   * 这个类**不可复用**：需要重新连接时由调用方新建一个实例（见各 app 的 store）。
   */
  close(): void {
    this.#disposed = true
    this.#clearTimers()
    const socket = this.#socket
    this.#socket = null
    if (socket !== null) {
      detach(socket)
      try {
        socket.close(1000, 'client-close')
      } catch {
        // 已经关掉的连接再关一次会抛；关闭路径不允许被打断。
      }
    }
    this.#setState('closed')
  }

  /* ---------------------------------------------------------------------- */
  /* 内部                                                                    */
  /* ---------------------------------------------------------------------- */

  #setState(next: RealtimeConnectionState): void {
    if (this.#state === next) return
    this.#state = next
    try {
      this.#options.onStateChange?.(next)
    } catch (cause) {
      // 订阅方（UI store）的异常不能影响连接本身。
      this.#logger.warn(`realtime: 状态回调抛错（${nameOf(cause)}）`)
    }
  }

  #clearTimers(): void {
    if (this.#reconnectTimer !== null) {
      clearTimeout(this.#reconnectTimer)
      this.#reconnectTimer = null
    }
    this.#stopHeartbeat()
  }

  #handleClose(closeCode?: number): void {
    // 心跳超时 / close() 已经处理过这条连接：那条路径把 #socket 置空并摘掉了回调，
    // 能走到这里就说明是本连接自己断的。
    if (this.#socket === null) return
    const socket = this.#socket
    this.#socket = null
    detach(socket)
    this.#stopHeartbeat()
    if (this.#disposed) {
      this.#setState('closed')
      return
    }
    this.#handleAttemptFailed(closeCode)
  }

  /** 一次尝试失败后的统一决策：要么退避重连，要么判定握手被拒并停下。 */
  #handleAttemptFailed(closeCode?: number): void {
    // 服务端在**升级成功之后**用自定义 close code 表达"拒绝"，浏览器能读到
    // （§74 的实现：4401 = 没有有效会话/账号停用，4403 = 会话属于另一个入口）。
    // 这是**确定**的鉴权失败信号：重连一万次也是同样的结果，必须立刻停下并让 UI
    // 提示重新登录——而不是继续指数退避。
    if (closeCode !== undefined && AUTH_CLOSE_CODES.includes(closeCode)) {
      this.#authFailed = true
      this.#setState('closed')
      this.#logger.warn(`realtime: 服务端以鉴权失败关闭实时通道（close code ${closeCode}）`)
      return
    }
    if (!this.#attemptOpened) {
      this.#handshakeFailures += 1
      if (
        this.#handshakeFailures >=
        (this.#options.handshakeFailureLimit ?? DEFAULTS.handshakeFailureLimit)
      ) {
        // 从未打开过 + 连续多次：再重试也只是打后端。停下来，把状态如实告诉 UI，
        // 由用户决定刷新/重新登录（低频兜底轮询仍然在工作，界面不会瞎）。
        this.#authFailed = true
        this.#setState('closed')
        this.#logger.warn('realtime: 多次未能建立实时通道，已停止自动重连')
        return
      }
    }
    this.#scheduleReconnect()
  }

  #scheduleReconnect(): void {
    if (this.#disposed || this.#reconnectTimer !== null) return
    const delay = this.#nextDelay()
    this.#attempt += 1
    // 退避等待期间也报 'connecting'：对 UI 而言"还没通、正在努力"是同一个状态，
    // 让界面显示"正在重连…"而不是一闪一闪的"已断开"。
    this.#setState('connecting')
    this.#reconnectTimer = setTimeout(() => {
      this.#reconnectTimer = null
      this.connect()
    }, delay)
  }

  /**
   * 第 n 次重连的等待时间：`min(max, base × 2^n)` 再加 0–20% 抖动。
   *
   * 抖动的作用是打散"整个班同时掉线 → 同一毫秒一起重连"的尖峰（§52 的网络纪律）。
   * 上限 30 秒：更长的等待就意味着用户宁愿手动刷新页面了。
   */
  #nextDelay(): number {
    const base = this.#options.reconnectBaseMs ?? DEFAULTS.reconnectBaseMs
    const max = this.#options.reconnectMaxMs ?? DEFAULTS.reconnectMaxMs
    const jitterRatio = this.#options.reconnectJitterRatio ?? DEFAULTS.reconnectJitterRatio
    const random = this.#options.random ?? Math.random
    const exponential = Math.min(max, base * 2 ** Math.min(this.#attempt, 16))
    const jitter = exponential * jitterRatio * random()
    return Math.min(max, Math.round(exponential + jitter))
  }

  #startHeartbeat(): void {
    this.#stopHeartbeat()
    const interval = this.#options.heartbeatIntervalMs ?? DEFAULTS.heartbeatIntervalMs
    this.#sendPing()
    this.#heartbeatTimer = setInterval(() => {
      this.#sendPing()
    }, interval)
  }

  #stopHeartbeat(): void {
    if (this.#heartbeatTimer !== null) {
      clearInterval(this.#heartbeatTimer)
      this.#heartbeatTimer = null
    }
    if (this.#pongTimer !== null) {
      clearTimeout(this.#pongTimer)
      this.#pongTimer = null
    }
    this.#awaitingPong = false
  }

  #sendPing(): void {
    const socket = this.#socket
    if (socket === null) return
    if (this.#awaitingPong) return
    try {
      socket.send(JSON.stringify({ type: REALTIME_PING_TYPE }))
    } catch {
      // 发送失败说明连接已经坏了：onclose 会接手（假 socket 里也由超时兜底）。
      return
    }
    this.#awaitingPong = true
    this.#pongTimer = setTimeout(() => {
      this.#handleHeartbeatTimeout()
    }, this.#options.heartbeatTimeoutMs ?? DEFAULTS.heartbeatTimeoutMs)
  }

  /**
   * 心跳超时：**主动**断掉并重连。
   *
   * WHY 不能只记录一句日志：TCP 连接可以处于"看起来还在、实际已经不通"的假死状态
   * （NAT 超时、中间设备丢状态）。不主动断开的话，UI 会一直显示"已连接实时通道"，
   * 而事件早就收不到了——这正是"断线期间不能假装一切正常"要防的那种情况。
   */
  #handleHeartbeatTimeout(): void {
    const socket = this.#socket
    this.#socket = null
    this.#stopHeartbeat()
    if (socket !== null) {
      detach(socket)
      try {
        socket.close(4000, 'heartbeat-timeout')
      } catch {
        // 关闭失败不影响后续判定：本地已经当成断了。
      }
    }
    this.#logger.warn('realtime: 心跳超时，判定连接已断开')
    if (this.#disposed) return
    this.#handleAttemptFailed()
  }

  /** 收到任何一帧都说明链路活着（不只是 PONG：事件同样是证据）。 */
  #markAlive(): void {
    this.#awaitingPong = false
    if (this.#pongTimer !== null) {
      clearTimeout(this.#pongTimer)
      this.#pongTimer = null
    }
  }

  #handleMessage(raw: unknown): void {
    this.#markAlive()
    if (typeof raw !== 'string') {
      this.#dropMessage('非文本报文')
      return
    }
    let parsed: unknown
    try {
      parsed = JSON.parse(raw)
    } catch {
      this.#dropMessage('JSON 解析失败')
      return
    }
    if (isPong(parsed)) return
    if (!isRealtimeEvent(parsed)) {
      this.#dropMessage('未知或结构不符的事件')
      return
    }
    try {
      this.#options.onEvent(parsed)
    } catch (cause) {
      // 一条事件处理失败不等于通道坏了：记下来，继续收发。
      this.#logger.warn(`realtime: 事件处理抛错（${nameOf(cause)}）`)
    }
  }

  /**
   * 丢弃一条无法使用的报文。
   *
   * 刻意**不打印报文内容**：里面可能有学生姓名、会话 id 等业务数据（§59 的日志纪律），
   * 而且坏掉的报文长度不可控。只留计数与原因，够定位"是不是服务端在发别的东西"。
   */
  #dropMessage(reason: string): void {
    this.#dropped += 1
    if (this.#dropped <= DROP_LOG_LIMIT) {
      this.#logger.warn(`realtime: 已忽略一条消息（${reason}），累计 ${this.#dropped} 条`)
    }
  }
}

function isPong(value: unknown): boolean {
  return (
    typeof value === 'object' &&
    value !== null &&
    (value as { type?: unknown }).type === REALTIME_PONG_TYPE
  )
}

function detach(socket: RealtimeSocketLike): void {
  socket.onopen = null
  socket.onmessage = null
  socket.onclose = null
  socket.onerror = null
}

/** 只取异常的名字：异常对象可能带着地址甚至凭据片段，绝不整体打印（§44）。 */
function nameOf(cause: unknown): string {
  return cause instanceof Error ? cause.name : typeof cause
}
