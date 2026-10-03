import {
  RealtimeSocket,
  type RealtimeSocketFactory,
  type RealtimeSocketOptions,
} from '@classwatch/api-client'

/**
 * 老师端实时通道的接线层（§47 / §74）。
 *
 * 与学生端（`apps/student-web/src/lib/realtime-channel.ts`）刻意各留一份极薄的实现：
 * 两端的**路径不同**（`/ws/teacher` 用的是老师会话 Cookie），而且两端对"通道状态
 * 该怎么说话"的要求也不同；共享的是真正复杂的那一层——重连、心跳、报文解析，
 * 它们在 `@classwatch/api-client` 里只有一份。
 *
 * 地址从 `window.location` 推导（开发走 Vite 代理的 `/ws`，生产走 nginx 反代，§62），
 * 因此代码里**没有端口**，也**没有任何凭据**：鉴权靠同源 Cookie（§38/§41）。
 */
export const TEACHER_REALTIME_PATH = '/ws/teacher'

export function realtimeChannelUrl(location: Location = window.location): string {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${location.host}${TEACHER_REALTIME_PATH}`
}

/** 测试注入点（默认 null = 浏览器原生 WebSocket），与其它适配层同一个模式。 */
let activeFactory: RealtimeSocketFactory | null = null

export function setRealtimeSocketFactory(factory: RealtimeSocketFactory | null): void {
  activeFactory = factory
}

export function createRealtimeChannel(
  options: Omit<RealtimeSocketOptions, 'url' | 'socketFactory'>,
): RealtimeSocket {
  return new RealtimeSocket({
    ...options,
    url: realtimeChannelUrl(),
    ...(activeFactory === null ? {} : { socketFactory: activeFactory }),
  })
}
