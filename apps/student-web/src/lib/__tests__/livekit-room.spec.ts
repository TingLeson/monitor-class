import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createLiveKitScreenPublisherRoom } from '../media/livekit-room.ts'

/**
 * 真实 SDK 适配层的钉桩测试（§20 / §26 / §28 / §44）。
 *
 * 这一份测试**唯一**的目的是把"我们到底怎么用 LiveKit"钉死：
 *
 * - `connect(url, token, { autoSubscribe: false })`（§26/§28 的硬要求）；
 * - `LocalVideoTrack(track, undefined, true)` —— `userProvidedTrack = true`
 *   表示这条轨道由应用提供，SDK 不得自己释放或重新采集（§20 复用同一条轨道的
 *   SDK 层表达）；
 * - `publishTrack(localTrack, { source: Track.Source.ScreenShare })`；
 * - `unpublishTrack(track, false)` / `disconnect(false)` —— 本地轨道由捕获层释放；
 * - 只用官方非 deprecated 的成员（`RemoteTrackPublication.setSubscribed` 在老师端，
 *   学生端这里不出现 `createLocalScreenTracks()` / `getDisplayMedia()`）。
 *
 * 业务性质（谁在什么时候 publish）由 store 的测试覆盖；这里覆盖"调用形状"。
 * 两者分开的理由：SDK 升级时坏的通常是这一份，而业务逻辑不该跟着一起红。
 */

interface RecordedRoom {
  localParticipant: { connectionQuality: string }
}

const lk = vi.hoisted(() => ({
  roomOptions: [] as Record<string, unknown>[],
  connectCalls: [] as { url: string; token: string; options: Record<string, unknown> }[],
  disconnectCalls: [] as (boolean | undefined)[],
  publishCalls: [] as { track: unknown; options: Record<string, unknown> }[],
  unpublishCalls: [] as { track: unknown; stop: boolean | undefined }[],
  localVideoTrackArgs: [] as unknown[][],
  listeners: new Map<string, Set<(...args: never[]) => void>>(),
  instances: [] as RecordedRoom[],
}))

vi.mock('livekit-client', () => {
  class LocalVideoTrack {
    readonly kind = 'video'
    constructor(...args: unknown[]) {
      lk.localVideoTrackArgs.push(args)
    }
  }

  class Room {
    readonly localParticipant = {
      connectionQuality: 'good',
      trackPublications: new Map<string, unknown>(),
      publishTrack: (track: unknown, options: Record<string, unknown>) => {
        lk.publishCalls.push({ track, options })
        return Promise.resolve({ trackSid: 'TR_published' })
      },
      unpublishTrack: (track: unknown, stop?: boolean) => {
        lk.unpublishCalls.push({ track, stop })
        return Promise.resolve(undefined)
      },
    }

    readonly remoteParticipants = new Map<string, unknown>()

    constructor(options: Record<string, unknown>) {
      lk.roomOptions.push(options)
      lk.instances.push(this as unknown as RecordedRoom)
    }

    connect(url: string, token: string, options: Record<string, unknown>): Promise<void> {
      lk.connectCalls.push({ url, token, options })
      return Promise.resolve()
    }

    disconnect(stop?: boolean): Promise<void> {
      lk.disconnectCalls.push(stop)
      return Promise.resolve()
    }

    on(event: string, handler: (...args: never[]) => void): this {
      const set = lk.listeners.get(event) ?? new Set()
      set.add(handler)
      lk.listeners.set(event, set)
      return this
    }

    off(event: string, handler: (...args: never[]) => void): this {
      lk.listeners.get(event)?.delete(handler)
      return this
    }

    getParticipantByIdentity(identity: string): unknown {
      return this.remoteParticipants.get(identity)
    }
  }

  return {
    Room,
    LocalVideoTrack,
    RoomEvent: {
      ConnectionQualityChanged: 'connectionQualityChanged',
      Disconnected: 'disconnected',
      Reconnecting: 'reconnecting',
      Reconnected: 'reconnected',
    },
    ConnectionQuality: {
      Excellent: 'excellent',
      Good: 'good',
      Poor: 'poor',
      Lost: 'lost',
      Unknown: 'unknown',
    },
    // 数字枚举的替身：适配层用反向映射取名字。
    DisconnectReason: {
      0: 'UNKNOWN_REASON',
      4: 'CLIENT_INITIATED',
      UNKNOWN_REASON: 0,
      CLIENT_INITIATED: 4,
    },
    Track: { Source: { ScreenShare: 'screen_share', Camera: 'camera', Microphone: 'microphone' } },
  }
})

/** 派发一个房间事件（模拟 SDK 的回调）。 */
function emit(event: string, ...args: unknown[]): void {
  for (const handler of [...(lk.listeners.get(event) ?? [])]) {
    ;(handler as (...rest: unknown[]) => void)(...args)
  }
}

const CREDENTIALS = {
  livekitUrl: 'wss://classwatch-test.livekit.cloud',
  token: 'short-lived-token',
}

describe('LiveKit 适配层（学生端）', () => {
  beforeEach(() => {
    lk.roomOptions.length = 0
    lk.connectCalls.length = 0
    lk.disconnectCalls.length = 0
    lk.publishCalls.length = 0
    lk.unpublishCalls.length = 0
    lk.localVideoTrackArgs.length = 0
    lk.listeners.clear()
    lk.instances.length = 0
  })

  it('§26/§28：connect 必须显式 autoSubscribe=false（学生不订阅任何人的轨道）', async () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)

    await room.connect()

    expect(lk.connectCalls).toHaveLength(1)
    expect(lk.connectCalls[0]?.url).toBe('wss://classwatch-test.livekit.cloud')
    expect(lk.connectCalls[0]?.token).toBe('short-lived-token')
    expect(lk.connectCalls[0]?.options).toEqual({ autoSubscribe: false })
  })

  it('§20/§52：房间选项使用 SDK 既有能力，并声明"本地轨道由应用管理"', () => {
    createLiveKitScreenPublisherRoom(CREDENTIALS)

    expect(lk.roomOptions[0]).toEqual({
      // 自适应流按附着元素尺寸选层、不可见时暂停下行（§52：用 LiveKit 已有能力）。
      adaptiveStream: true,
      dynacast: true,
      // 本地轨道由捕获层释放：unpublish 时 SDK 不能顺手 stop 掉 Phase 5 那条轨道。
      stopLocalTrackOnUnpublish: false,
      // 关页面时 SDK 自己尽力断开（我们另有 beforeunload 的同步兜底）。
      disconnectOnPageLeave: true,
    })
  })

  it('§20：publish 用 LocalVideoTrack 包装**同一条** MediaStreamTrack，且标记为应用提供', async () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)
    const track = { kind: 'video' } as unknown as MediaStreamTrack

    await room.publishScreenTrack(track)

    // 第三个参数 true = userProvidedTrack：SDK 不得重新采集（否则学生会第二次被弹授权框）。
    expect(lk.localVideoTrackArgs).toEqual([[track, undefined, true]])
    expect(lk.publishCalls).toHaveLength(1)
    expect(lk.publishCalls[0]?.options).toEqual({ source: 'screen_share' })
  })

  it('适配层不调用任何"重新采集屏幕"的 API（不存在第二条 Gate 绕过路径）', () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)

    // 接口面上根本没有这些入口：一旦有人加回来，这个断言会连同类型检查一起失败。
    expect(Object.keys(room).sort()).toEqual([
      'connect',
      'connectionQuality',
      'disconnect',
      'onDisconnected',
      'onQualityChanged',
      'onReconnected',
      'onReconnecting',
      'publishCameraTrack',
      'publishScreenTrack',
      'unpublishCameraTrack',
      'unpublishScreenTrack',
    ])
  })

  it('§24：摄像头用同一个 LocalVideoTrack 包装方式发布到 Track.Source.Camera', async () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)
    const track = { kind: 'video' } as unknown as MediaStreamTrack

    await room.publishCameraTrack(track)

    // 第三个参数 true = userProvidedTrack：SDK 不得在重连时自己重新采集摄像头
    // （那等于学生在课堂中途被弹第二次授权框，§24 的前提就是只请求一次）。
    expect(lk.localVideoTrackArgs).toEqual([[track, undefined, true]])
    expect(lk.publishCalls).toHaveLength(1)
    // source 是判定归属的字段（后端 webhook 也只看它）；name 是 §75 契约里那条轨道的名字。
    expect(lk.publishCalls[0]?.options).toEqual({ source: 'camera', name: 'camera' })
  })

  it('§24：摄像头与屏幕是两条独立的发布，撤下其中一条不影响另一条', async () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)
    const screen = { kind: 'video' } as unknown as MediaStreamTrack
    const camera = { kind: 'video' } as unknown as MediaStreamTrack
    await room.publishScreenTrack(screen)
    await room.publishCameraTrack(camera)

    await room.unpublishCameraTrack()

    expect(lk.unpublishCalls).toEqual([{ track: expect.anything(), stop: false }])
    // 屏幕那条仍然在发布（unpublish 只发生在摄像头上）。
    await room.unpublishScreenTrack()
    expect(lk.unpublishCalls).toHaveLength(2)
    expect(lk.unpublishCalls[0]?.track).not.toBe(lk.unpublishCalls[1]?.track)
  })

  it('没有发布过摄像头时 unpublish 是空操作', async () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)

    await room.unpublishCameraTrack()

    expect(lk.unpublishCalls).toHaveLength(0)
  })

  it('unpublish / disconnect 都不停止本地轨道（stopTracks=false）', async () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)
    const track = { kind: 'video' } as unknown as MediaStreamTrack
    await room.publishScreenTrack(track)

    await room.unpublishScreenTrack()
    await room.disconnect()

    expect(lk.unpublishCalls).toHaveLength(1)
    expect(lk.unpublishCalls[0]?.stop).toBe(false)
    expect(lk.disconnectCalls).toEqual([false])
  })

  it('没有发布过轨道时 unpublish 是空操作（不要对 null 调 SDK）', async () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)

    await room.unpublishScreenTrack()

    expect(lk.unpublishCalls).toHaveLength(0)
  })

  it('连接质量只转发**本端**参与者，并映射成我们的等级类型', () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)
    const local = lk.instances[0]?.localParticipant
    const seen: string[] = []
    room.onQualityChanged((quality) => seen.push(quality))

    // 别的参与者（Phase 10 的老师语音、其他学生的媒体）与我们无关（§26）。
    emit('connectionQualityChanged', 'poor', { identity: 'someone-else' })
    expect(seen).toEqual([])

    emit('connectionQualityChanged', 'excellent', local)
    emit('connectionQualityChanged', 'poor', local)
    emit('connectionQualityChanged', 'lost', local)
    emit('connectionQualityChanged', 'unknown', local)
    expect(seen).toEqual(['excellent', 'poor', 'lost', 'unknown'])
  })

  it('connectionQuality() 读的是本端当前值（初始 unknown 不冒充 good）', () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)

    expect(room.connectionQuality()).toBe('good')
    const local = lk.instances[0]?.localParticipant
    if (local) local.connectionQuality = 'lost'
    expect(room.connectionQuality()).toBe('lost')
  })

  it('断开事件带可读原因，取消订阅后不再回调', () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)
    const reasons: (string | null)[] = []
    const off = room.onDisconnected((reason) => reasons.push(reason))

    emit('disconnected', 4)
    emit('disconnected', undefined)
    off()
    emit('disconnected', 0)

    // 数字枚举被换成名字，排障时能看出"是客户端主动断的还是服务端踢的"。
    expect(reasons).toEqual(['CLIENT_INITIATED', null])
  })

  it('重连事件：onReconnecting / onReconnected 各自可取消订阅', () => {
    const room = createLiveKitScreenPublisherRoom(CREDENTIALS)
    let reconnecting = 0
    let reconnected = 0
    const offReconnecting = room.onReconnecting(() => {
      reconnecting += 1
    })
    room.onReconnected(() => {
      reconnected += 1
    })

    emit('reconnecting')
    emit('reconnected')
    offReconnecting()
    emit('reconnecting')

    expect(reconnecting).toBe(1)
    expect(reconnected).toBe(1)
  })
})
