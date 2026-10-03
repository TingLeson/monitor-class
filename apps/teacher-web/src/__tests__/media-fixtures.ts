import { afterEach, vi } from 'vitest'

/**
 * 老师端媒体设备替身（§27 / §31，Phase 10）。
 *
 * WHY 老师端只需要**麦克风**这一半：§27 的 Teacher Token 里 `canPublishSources`
 * 只有 `microphone`，所以老师端永远不会请求摄像头或屏幕。与学生端的
 * `screen-fixtures.ts` 相比，这里少掉的一切都是"老师端不存在的能力"，
 * 而不是"还没写"。
 */

/** 与真实 MediaStreamTrack 中老师端用到的那部分一致。 */
export interface FakeAudioTrack {
  kind: 'audio'
  readyState: string
  enabled: boolean
  onended: ((event: Event) => void) | null
  stop(): void
  addEventListener(type: string, listener: () => void): void
  removeEventListener(type: string, listener: () => void): void
  /** 测试专用：记录停了几次（"关闭麦克风必须真的 stop"就靠它）。 */
  readonly stopCalls: number
  /** 测试专用：按真实浏览器的方式派发 ended。 */
  emitEnded(): void
}

export interface FakeAudioStream {
  active: boolean
  getTracks(): FakeAudioTrack[]
  getAudioTracks(): FakeAudioTrack[]
}

export function makeFakeAudioTrack(): FakeAudioTrack {
  const listeners = new Set<() => void>()
  let stopCalls = 0
  const track: FakeAudioTrack = {
    kind: 'audio',
    readyState: 'live',
    enabled: true,
    onended: null,
    get stopCalls() {
      return stopCalls
    },
    stop(): void {
      stopCalls += 1
      track.readyState = 'ended'
    },
    addEventListener(type: string, listener: () => void): void {
      if (type === 'ended') listeners.add(listener)
    },
    removeEventListener(type: string, listener: () => void): void {
      if (type === 'ended') listeners.delete(listener)
    },
    emitEnded(): void {
      track.readyState = 'ended'
      for (const listener of [...listeners]) listener()
    },
  }
  return track
}

/**
 * 一条麦克风流：必须是**真实的 `MediaStream` 实例**。
 *
 * happy-dom 漏实现了 `getTracks()` 之外的按 kind 过滤方法，这里按规范补上
 * （与学生端摄像头/麦克风替身同一套做法）。
 */
export function makeFakeAudioStream(
  tracks: FakeAudioTrack[] = [makeFakeAudioTrack()],
): FakeAudioStream {
  const stream = new MediaStream(tracks as unknown as MediaStreamTrack[])
  Object.defineProperty(stream, 'getTracks', { value: () => [...tracks], configurable: true })
  Object.defineProperty(stream, 'getAudioTracks', {
    value: () => tracks.filter((track) => track.kind === 'audio'),
    configurable: true,
  })
  return stream as unknown as FakeAudioStream
}

export interface TeacherMediaDevicesStub {
  /** 每一次 `getUserMedia` 的入参（长度本身就是"有没有偷偷请求设备"的答案）。 */
  micCalls: { constraints: unknown }[]
  /** 每次返回的麦克风流（与 micCalls 一一对应）。 */
  micStreams: FakeAudioStream[]
  /** 让 `getUserMedia` 抛出这个错误（权限被拒 / 设备被占用）。 */
  micFailWith: Error | null
  getUserMedia(constraints?: unknown): Promise<unknown>
}

export function makeTeacherMediaDevicesStub(): TeacherMediaDevicesStub {
  const stub: TeacherMediaDevicesStub = {
    micCalls: [],
    micStreams: [],
    micFailWith: null,
    getUserMedia(constraints?: unknown): Promise<unknown> {
      stub.micCalls.push({ constraints })
      if (stub.micFailWith) return Promise.reject(stub.micFailWith)
      const stream = makeFakeAudioStream()
      stub.micStreams.push(stream)
      return Promise.resolve(stream)
    },
  }
  return stub
}

/** 把替身装到 `navigator.mediaDevices` 上（happy-dom 默认没有它）。 */
export function installTeacherMediaDevices(devices: unknown): void {
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    writable: true,
    value: devices,
  })
}

export function cleanupTeacherMediaDevices(): void {
  Reflect.deleteProperty(navigator, 'mediaDevices')
}

/** 第 N 次（默认第一次）请求造出来的麦克风轨道。 */
export function micTrackOf(devices: TeacherMediaDevicesStub, index = 0): FakeAudioTrack {
  const track = devices.micStreams[index]?.getAudioTracks()[0]
  if (!track) throw new Error(`假浏览器没有产出第 ${index} 条麦克风轨道`)
  return track
}

/** 自动清理：任何安装过 mediaDevices 的测试都不会污染下一个。 */
afterEach(() => {
  cleanupTeacherMediaDevices()
  vi.restoreAllMocks()
})
