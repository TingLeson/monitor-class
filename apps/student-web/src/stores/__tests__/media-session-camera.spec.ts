import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { installFakePublisherRoom, makeJoinResponse } from '../../__tests__/media-fixtures'
import { makeFakeCapture } from '../../__tests__/screen-fixtures'
import {
  installMediaDevices,
  makeMediaDevicesStub,
  type FakeTrack,
  type MediaDevicesStub,
} from '../../__tests__/screen-fixtures'
import { makeOpenStudentClassroom } from '../../__tests__/fixtures'

/**
 * 摄像头 store 测试（§21 / §24 / §75）。
 *
 * 这一份盯的是"摄像头这条支线**永远不会**碰课堂主状态"：
 *
 * 1. 没点之前一次设备请求都没有（§24：进入课堂之后才请求）；
 * 2. 打开 = publish 一条 `camera` 轨道；关闭 = unpublish **加**真的 stop
 *    （后者是唯一能让摄像头指示灯灭掉的动作）；
 * 3. 开 / 关 / 再开 是三次独立的设备请求（不能复用一条已停的轨道）；
 * 4. 摄像头失败只改 `cameraState` / `cameraFailure`——phase、屏幕轨道、
 *    课堂状态一个都不许动（§21：屏幕才是 mandatory）；
 * 5. 屏幕丢了、媒体断了、离开课堂：摄像头跟着该走走、该留留。
 */

const { joinClassroomMock, leaveSessionMock, getClassroomMock } = vi.hoisted(() => ({
  joinClassroomMock: vi.fn(),
  leaveSessionMock: vi.fn(),
  getClassroomMock: vi.fn(),
}))

vi.mock('../../lib/student-sessions-api.ts', () => ({
  joinClassroom: joinClassroomMock,
  leaveSession: leaveSessionMock,
}))

vi.mock('../../lib/student-classrooms-api.ts', () => ({
  listClassrooms: vi.fn(),
  getClassroom: getClassroomMock,
}))

const OPEN = makeOpenStudentClassroom({ id: 'room-open', name: 'C++ 算法训练' })

/** 进课堂（prepare + begin），并把假浏览器装好。 */
async function startSession(options: { devices?: MediaDevicesStub } = {}) {
  const { useMediaSessionStore } = await import('../media-session.ts')
  const store = useMediaSessionStore()
  const devices = options.devices ?? makeMediaDevicesStub()
  installMediaDevices(devices)
  const capture = makeFakeCapture()
  store.prepare({
    sessionId: 'session-1',
    classroomId: 'room-open',
    credentials: { livekitUrl: 'wss://classwatch-test.livekit.cloud', token: 'fake-token' },
    capture,
  })
  getClassroomMock.mockResolvedValue(OPEN)
  await store.begin()
  return { store, capture, devices }
}

/** 第 N 次（默认第一次）getUserMedia 造出来的摄像头轨道。 */
function cameraTrackOf(devices: MediaDevicesStub, index = 0): FakeTrack {
  const track = devices.cameraStreams[index]?.getVideoTracks()[0]
  if (!track) throw new Error(`假浏览器没有产出第 ${index} 条摄像头轨道`)
  return track
}

describe('课堂会话 store — 摄像头（§24）', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    joinClassroomMock.mockReset().mockResolvedValue(makeJoinResponse())
    leaveSessionMock.mockReset().mockResolvedValue(undefined)
    getClassroomMock.mockReset().mockResolvedValue(OPEN)
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('§24：进入课堂（begin）不会请求摄像头——挂载时 getUserMedia 必须是 0 次', async () => {
    installFakePublisherRoom()
    const { devices, store } = await startSession()

    expect(store.phase).toBe('online')
    expect(devices.cameraCalls).toHaveLength(0)
    expect(store.cameraState).toBe('off')
  })

  it('§24：点击才请求；成功后发布的正是那条 camera 轨道', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()

    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    expect(devices.cameraCalls).toHaveLength(1)
    expect(harness.current().publishedCameraTracks).toEqual([cameraTrackOf(devices)])
    // 屏幕那条轨道一次都没有被碰过：两条轨道互不影响。
    expect(harness.current().publishedTracks).toHaveLength(1)
    expect(store.isCameraOn).toBe(true)
    expect(store.cameraFailure).toBeNull()
  })

  it('§24：关闭 = unpublish + 真的 stop 本地轨道（摄像头指示灯必须灭）', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    store.toggleCamera()

    expect(harness.current().unpublishCameraCalls).toBe(1)
    expect(cameraTrackOf(devices).stopCalls).toBe(1)
    expect(cameraTrackOf(devices).readyState).toBe('ended')
    expect(store.cameraState).toBe('off')
    // 屏幕共享完全不受影响（§21 的强制规则不因为摄像头而改变）。
    expect(store.phase).toBe('online')
    expect(harness.current().publishedTracks).toHaveLength(1)
  })

  it('§24：关掉之后可以再开——再开是一次**新的** getUserMedia，而不是复用死轨道', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()

    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })
    store.toggleCamera()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    expect(devices.cameraCalls).toHaveLength(2)
    expect(harness.current().publishedCameraTracks).toEqual([
      cameraTrackOf(devices, 0),
      cameraTrackOf(devices, 1),
    ])
    // 第一条轨道已经死了，绝不能再发布它。
    expect(cameraTrackOf(devices, 0).readyState).toBe('ended')
    expect(cameraTrackOf(devices, 1).readyState).toBe('live')
  })

  it('§24：权限被拒 → 可执行的中文提示，且课堂状态一点都没变', async () => {
    installFakePublisherRoom()
    const { store, devices, capture } = await startSession()
    const permissionError = new Error('denied')
    permissionError.name = 'NotAllowedError'
    devices.cameraFailWith = permissionError

    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('error')
    })

    expect(store.cameraFailure?.code).toBe('CAMERA_PERMISSION_DENIED')
    expect(store.cameraFailure?.message).toContain('仍然可以正常上课')
    // 下面这几条就是"摄像头失败绝不影响课堂会话"的全部内容。
    expect(store.phase).toBe('online')
    expect(store.hasFailure).toBe(false)
    expect(capture.track.readyState).toBe('live')
    expect(store.isCameraOn).toBe(false)
  })

  it('§24：设备被占用 → 提示"可能被其他程序占用"，同样不影响课堂', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    const busy = new Error('busy')
    busy.name = 'NotReadableError'
    devices.cameraFailWith = busy

    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('error')
    })

    expect(store.cameraFailure?.kind).toBe('device-busy')
    expect(store.cameraFailure?.message).toContain('占用')
    expect(store.phase).toBe('online')
  })

  it('publish 失败：不留下一条亮着灯的本地轨道', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    harness.flags.publishCameraError = new Error('publish rejected')

    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('error')
    })

    expect(devices.cameraCalls).toHaveLength(1)
    expect(cameraTrackOf(devices).stopCalls).toBe(1)
    expect(store.cameraStream).toBeNull()
  })

  it('§21/§24：屏幕中断不影响摄像头，摄像头开着也不会掩盖"已停止屏幕共享"', async () => {
    installFakePublisherRoom()
    const { store, capture } = await startSession()

    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    capture.emitEnded()
    await Promise.resolve()

    // 屏幕那条支线进入提示态（界面必须显示 ⚠），而摄像头是另一件事。
    expect(store.phase).toBe('screen-lost')
    expect(store.isScreenLost).toBe(true)
    expect(store.cameraState).toBe('on')
  })

  it('§21：关闭摄像头时撤下的只是摄像头轨道，屏幕那条继续进行', async () => {
    const harness = installFakePublisherRoom()
    const { store } = await startSession()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    store.toggleCamera()
    await Promise.resolve()

    expect(harness.current().unpublishCalls).toBe(0)
    expect(store.phase).toBe('online')
    expect(store.isOnline).toBe(true)
  })

  it('§24：设备中途被拔出（track ended）→ 回到 error 并释放，不留假"已开启"', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    cameraTrackOf(devices).emitEnded()

    expect(store.cameraState).toBe('error')
    expect(store.cameraFailure?.kind).toBe('track-ended')
    expect(store.cameraStream).toBeNull()
    /**
     * 死轨道还必须被 unpublish：否则 SFU 里留着一条没人推流的 camera 发布，
     * 服务端的 webhook 永远不会说"摄像头停了"，老师那端的画中画会钉在最后一帧。
     */
    expect(harness.current().unpublishCameraCalls).toBeGreaterThanOrEqual(1)
  })

  it('§24：媒体连接断开 → 摄像头被释放（"已开启"不能在没有下行时继续显示）', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    harness.current().emitDisconnected()

    expect(store.phase).toBe('media-error')
    expect(store.cameraState).toBe('off')
    expect(cameraTrackOf(devices).readyState).toBe('ended')
  })

  it('§24/§65 Case 14：离开课堂必须停掉摄像头（不在后台留着设备占用）', async () => {
    const harness = installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    await store.leave()

    expect(cameraTrackOf(devices).readyState).toBe('ended')
    expect(store.cameraState).toBe('off')
    expect(store.cameraStream).toBeNull()
    expect(harness.current().disconnectCalls).toBe(1)
  })

  it('§24：releaseNow（beforeunload 的同步兜底）也要同步停掉摄像头', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    store.releaseNow()

    // 卸载路径上异步断开很可能跑不完，但 stop() 是同步的——灯必须立刻灭。
    expect(cameraTrackOf(devices).readyState).toBe('ended')
    expect(store.cameraState).toBe('off')
  })

  it('§49：老师关闭课堂 → 摄像头一并释放（不给一节已经结束的课留着摄像头）', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    const { makeRoomClosedEvent } = await import('../../__tests__/realtime-fixtures.ts')
    store.applyRealtimeEvent(makeRoomClosedEvent({ classroomId: 'room-open' }))

    expect(store.phase).toBe('closed')
    expect(cameraTrackOf(devices).readyState).toBe('ended')
    expect(store.cameraState).toBe('off')
  })

  it('§24：还没进入课堂时不申请设备权限（没有房间可发布）', async () => {
    installFakePublisherRoom()
    const { useMediaSessionStore } = await import('../media-session.ts')
    const store = useMediaSessionStore()
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)

    // 还没有 prepare / begin：连房间都没有。
    expect(store.canUseCamera).toBe(false)
    store.toggleCamera()
    await Promise.resolve()

    expect(devices.cameraCalls).toHaveLength(0)
    expect(store.cameraState).toBe('off')
  })

  it('§24：连点两下不会发出第二次设备请求（requesting 期间的重入保护）', async () => {
    installFakePublisherRoom()
    const { store, devices } = await startSession()

    store.toggleCamera()
    store.toggleCamera()
    store.toggleCamera()
    await vi.waitFor(() => {
      expect(store.cameraState).toBe('on')
    })

    // 第一下开启，后两下分别落在 requesting 与 on 上：两次都是空操作。
    expect(devices.cameraCalls).toHaveLength(1)
  })
})
