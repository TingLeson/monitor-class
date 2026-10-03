import { afterEach, vi } from 'vitest'
import {
  resetScreenPublisherRoomFactory,
  setScreenPublisherRoomFactory,
  type ConnectionQualityLevel,
  type MediaCredentials,
  type MediaDisconnectReason,
  type ScreenPublisherRoom,
} from '../lib/media/media-room.ts'

/**
 * 学生端媒体测试替身（§64 Frontend Test）。
 *
 * WHY 替身挂在 `media-room.ts` 的工厂注入点上，而不是 `vi.mock('livekit-client')`：
 * 真实 SDK 在 import 期就会触碰浏览器 API（webrtc-adapter 等），把它拉进 happy-dom
 * 既慢又脆；而我们要断言的也不是"SDK 被怎么调用"，而是**业务性质**——
 * publish 的是不是 Phase 5 那条轨道、有没有第二次 join、订阅有没有重复。
 * 那些性质在没有 SDK 的情况下反而断言得更准确。
 *
 * SDK 本身的调用形状由 `src/lib/__tests__/livekit-room.spec.ts` 用模块替身单独钉住。
 */

/** 一个受控的假房间：记录所有调用，并允许测试主动派发事件。 */
export interface FakePublisherRoom {
  /** 交给 store 的那个对象（接口与真实房间完全一致）。 */
  readonly room: ScreenPublisherRoom
  /** 连接次数（断言"没有第二次 join"时数这个）。 */
  readonly connectCalls: number
  /** 发布过的屏幕轨道，按顺序排列。 */
  readonly publishedTracks: MediaStreamTrack[]
  /** 发布过的摄像头轨道（§24）。与屏幕**分开记**：两条轨道必须独立开关。 */
  readonly publishedCameraTracks: MediaStreamTrack[]
  readonly unpublishCalls: number
  /** 撤下摄像头的次数（断言"关闭 = unpublish + stop"里的前一半）。 */
  readonly unpublishCameraCalls: number
  readonly disconnectCalls: number
  /**
   * 让接下来的 connect / publish 失败。
   *
   * 读写的是**整个 harness 共享**的开关（见 `PublisherRoomHarness.flags`），
   * 因为重试会创建一个新房间：只改旧房间上的标志，重试照样会失败。
   */
  connectError: Error | null
  publishError: Error | null
  /** 发布摄像头时的失败开关（与屏幕分开：摄像头失败不能污染屏幕那条路径）。 */
  publishCameraError: Error | null
  emitQuality(quality: ConnectionQualityLevel): void
  emitDisconnected(reason?: MediaDisconnectReason): void
  emitReconnecting(): void
  emitReconnected(): void
}

export interface PublisherRoomHarness {
  /** 按创建顺序排列的假房间（重试会创建第二个）。 */
  readonly rooms: FakePublisherRoom[]
  /** 工厂收到的每一份凭据。断言"重试确实重新 join 了"时看这里。 */
  readonly credentials: MediaCredentials[]
  /** 共享的开关：改它会影响**之后创建**的房间（重试场景要用）。 */
  readonly flags: {
    connectError: Error | null
    publishError: Error | null
    publishCameraError: Error | null
    /**
     * 让 publish 在"成功"的同时把轨道结束掉。
     *
     * 模拟 §22 里最刁钻的一个窗口：轨道在 publish 飞行期间被浏览器结束。
     * 这时绝不能宣布 online——老师那端会看到一个永远黑屏的"在线"学生。
     */
    endTrackOnPublish: boolean
  }
  /** 最近创建的房间；还没创建过时抛错（比 ??? 更容易定位）。 */
  current(): FakePublisherRoom
}

/**
 * 装上假工厂。
 *
 * 每个测试结束后由本文件的 afterEach 还原：假工厂一旦泄漏到下一个测试文件，
 * 那个文件里的"真实连接失败"会变成一个永远不会发生的分支。
 */
export function installFakePublisherRoom(
  options: { connectError?: Error; publishError?: Error } = {},
): PublisherRoomHarness {
  const rooms: FakePublisherRoom[] = []
  const credentials: MediaCredentials[] = []
  const flags = {
    connectError: options.connectError ?? null,
    publishError: options.publishError ?? null,
    publishCameraError: null as Error | null,
    endTrackOnPublish: false,
  }

  setScreenPublisherRoomFactory((nextCredentials) => {
    credentials.push(nextCredentials)
    const room = makeFakePublisherRoom(flags)
    rooms.push(room)
    return Promise.resolve(room.room)
  })

  return {
    rooms,
    credentials,
    flags,
    current(): FakePublisherRoom {
      const last = rooms[rooms.length - 1]
      if (!last) throw new Error('还没有创建任何假房间')
      return last
    },
  }
}

function makeFakePublisherRoom(flags: {
  connectError: Error | null
  publishError: Error | null
  publishCameraError: Error | null
  endTrackOnPublish: boolean
}): FakePublisherRoom {
  const qualityListeners: ((quality: ConnectionQualityLevel) => void)[] = []
  const disconnectedListeners: ((reason: MediaDisconnectReason) => void)[] = []
  const reconnectingListeners: (() => void)[] = []
  const reconnectedListeners: (() => void)[] = []

  const publishedTracks: MediaStreamTrack[] = []
  const publishedCameraTracks: MediaStreamTrack[] = []
  let connectCalls = 0
  let unpublishCalls = 0
  let unpublishCameraCalls = 0
  let disconnectCalls = 0
  let quality: ConnectionQualityLevel = 'good'

  const self: FakePublisherRoom = {
    room: {
      connect(): Promise<void> {
        connectCalls += 1
        if (flags.connectError) return Promise.reject(flags.connectError)
        return Promise.resolve()
      },
      publishScreenTrack(track: MediaStreamTrack): Promise<void> {
        if (flags.publishError) return Promise.reject(flags.publishError)
        publishedTracks.push(track)
        if (flags.endTrackOnPublish) track.stop()
        return Promise.resolve()
      },
      unpublishScreenTrack(): Promise<void> {
        unpublishCalls += 1
        return Promise.resolve()
      },
      publishCameraTrack(track: MediaStreamTrack): Promise<void> {
        if (flags.publishCameraError) return Promise.reject(flags.publishCameraError)
        publishedCameraTracks.push(track)
        return Promise.resolve()
      },
      unpublishCameraTrack(): Promise<void> {
        unpublishCameraCalls += 1
        return Promise.resolve()
      },
      disconnect(): Promise<void> {
        disconnectCalls += 1
        return Promise.resolve()
      },
      connectionQuality: () => quality,
      onQualityChanged(listener) {
        qualityListeners.push(listener)
        return () => removeFrom(qualityListeners, listener)
      },
      onDisconnected(listener) {
        disconnectedListeners.push(listener)
        return () => removeFrom(disconnectedListeners, listener)
      },
      onReconnecting(listener) {
        reconnectingListeners.push(listener)
        return () => removeFrom(reconnectingListeners, listener)
      },
      onReconnected(listener) {
        reconnectedListeners.push(listener)
        return () => removeFrom(reconnectedListeners, listener)
      },
    },
    get connectCalls() {
      return connectCalls
    },
    publishedTracks,
    publishedCameraTracks,
    get unpublishCalls() {
      return unpublishCalls
    },
    get unpublishCameraCalls() {
      return unpublishCameraCalls
    },
    get disconnectCalls() {
      return disconnectCalls
    },
    get connectError() {
      return flags.connectError
    },
    set connectError(next: Error | null) {
      flags.connectError = next
    },
    get publishError() {
      return flags.publishError
    },
    set publishError(next: Error | null) {
      flags.publishError = next
    },
    get publishCameraError() {
      return flags.publishCameraError
    },
    set publishCameraError(next: Error | null) {
      flags.publishCameraError = next
    },
    emitQuality(next) {
      quality = next
      for (const listener of [...qualityListeners]) listener(next)
    },
    emitDisconnected(reason: MediaDisconnectReason = null) {
      for (const listener of [...disconnectedListeners]) listener(reason)
    },
    emitReconnecting() {
      for (const listener of [...reconnectingListeners]) listener()
    },
    emitReconnected() {
      for (const listener of [...reconnectedListeners]) listener()
    },
  }
  return self
}

function removeFrom<T>(list: T[], item: T): void {
  const index = list.indexOf(item)
  if (index >= 0) list.splice(index, 1)
}

/** 自动还原：见 installFakePublisherRoom 的说明。 */
afterEach(() => {
  resetScreenPublisherRoomFactory()
  vi.restoreAllMocks()
})

/** 一份合法的 join 响应（§43）。token 是假的：它永远不该出现在界面或日志里。 */
export function makeJoinResponse(
  overrides: { sessionId?: string; livekitUrl?: string; token?: string } = {},
): { sessionId: string; livekitUrl: string; token: string } {
  return {
    sessionId: 'session-1',
    livekitUrl: 'wss://classwatch-test.livekit.cloud',
    token: 'fake-jwt-token',
    ...overrides,
  }
}
