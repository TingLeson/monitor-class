import {
  RealtimeSocket,
  type RealtimeSocketFactory,
  type RealtimeSocketOptions,
} from '@classwatch/api-client'

/**
 * 学生端实时通道的接线层（§47 / §74）。
 *
 * 只做两件事：**算出同源地址**、**提供测试注入点**。
 * 连接本身（重连、心跳、报文解析）由 `@classwatch/api-client` 的 `RealtimeSocket`
 * 实现——学生端与老师端对那件事的要求逐字相同，没有理由各写一遍。
 *
 * WHY 地址必须从 `window.location` 推导，而不是写一个常量：
 * 开发环境学生端在 5173、生产在 nginx 后面的同一个来源（§62），
 * 硬编码端口就等于"上线时再改一次代码"。这里按当前页面的协议选 ws / wss，
 * 按当前 host（含端口）拼路径，于是同一个构建在两种环境下都对。
 * **地址里没有任何凭据**：鉴权走同源 Cookie，浏览器在握手时自动带上（§38/§41）。
 */
export const STUDENT_REALTIME_PATH = '/ws/student'

export function realtimeChannelUrl(location: Location = window.location): string {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${location.host}${STUDENT_REALTIME_PATH}`
}

/**
 * 测试注入点（默认 null = 用浏览器原生 WebSocket）。
 *
 * 与 `media-room.ts` / `tile-visibility.ts` 的工厂注入同一个模式：真实实现里
 * 没有一行"测试专用分支"，而测试能精确控制何时 open / 收到什么 / 何时断开。
 */
let activeFactory: RealtimeSocketFactory | null = null

export function setRealtimeSocketFactory(factory: RealtimeSocketFactory | null): void {
  activeFactory = factory
}

/** 造一条通道（store 持有它，并在 HMR / 登出 / 卸载时 close）。 */
export function createRealtimeChannel(
  options: Omit<RealtimeSocketOptions, 'url' | 'socketFactory'>,
): RealtimeSocket {
  return new RealtimeSocket({
    ...options,
    url: realtimeChannelUrl(),
    ...(activeFactory === null ? {} : { socketFactory: activeFactory }),
  })
}
