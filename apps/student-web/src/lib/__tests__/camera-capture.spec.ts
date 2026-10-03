import { describe, expect, it } from 'vitest'
import {
  CameraCaptureError,
  CAMERA_CONSTRAINTS,
  checkCameraSupport,
  describeCameraFailure,
  releaseCameraCapture,
  requestCamera,
  toCameraFailure,
  toCameraFailureKind,
  toCameraFailureKindOf,
} from '../camera-capture'
import {
  installMediaDevices,
  makeFakeCameraTrack,
  makeMediaDevicesStub,
} from '../../__tests__/screen-fixtures'

/**
 * 摄像头采集层测试（§24 / §58 / §75）。
 *
 * 盯住三件事，每一件都对应一类真实故障：
 *
 * 1. **能力自检发生在请求之前**：没有 `getUserMedia` 时绝不"试一下再说"
 *    （那会弹出浏览器自己的英文报错）；
 * 2. **错误的分类必须正确**：权限被拒与设备被占用要给学生完全不同的下一步，
 *    混成一类就是让人反复点一个永远不会成功的按钮；
 * 3. **失败路径不留活轨道**：只要有一条 video track 还活着，系统摄像头指示灯就亮着，
 *    而学生会以为自己在被看着。
 */

/** 造一个带指定 error.name 的异常（浏览器抛的就是这种形状）。 */
function namedError(name: string): Error {
  const error = new Error(name)
  error.name = name
  return error
}

describe('摄像头能力自检（§17 的摄像头对应物）', () => {
  it('没有 navigator.mediaDevices → 不支持，原因为 no-mediaDevices', () => {
    expect(checkCameraSupport()).toEqual({ supported: false, reason: 'no-mediaDevices' })
  })

  it('有 mediaDevices 但没有 getUserMedia → 不支持（不能"试一下再说"）', () => {
    installMediaDevices({ getDisplayMedia: () => Promise.resolve(null) })

    expect(checkCameraSupport()).toEqual({ supported: false, reason: 'no-getUserMedia' })
  })

  it('支持时不做任何请求（纯读取检查，绝无副作用）', () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)

    expect(checkCameraSupport()).toEqual({ supported: true })
    expect(devices.cameraCalls).toHaveLength(0)
  })
})

describe('请求摄像头（§24）', () => {
  it('用 §24 冻结的 {video: true} 约束请求（不含 audio，麦克风属于 Phase 10）', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)

    const capture = await requestCamera()

    expect(devices.cameraCalls).toHaveLength(1)
    expect(devices.cameraCalls[0]?.constraints).toEqual({ video: true })
    expect(CAMERA_CONSTRAINTS).toEqual({ video: true })
    // 拿到的就是替身造出来的那条轨道（不是重新采集的一条）。
    expect(capture.track).toBe(devices.cameraStreams[0]?.getVideoTracks()[0])
  })

  it('不支持时直接拒绝，且**不**调用 getUserMedia', async () => {
    installMediaDevices({ getDisplayMedia: () => Promise.resolve(null) })

    await expect(requestCamera()).rejects.toBeInstanceOf(CameraCaptureError)
    await expect(requestCamera()).rejects.toMatchObject({ kind: 'unsupported' })
  })

  it('NotAllowedError（学生拒绝 / 系统未授权）→ permission-denied，且带 §58 的码', async () => {
    const devices = makeMediaDevicesStub()
    devices.cameraFailWith = namedError('NotAllowedError')
    installMediaDevices(devices)

    await expect(requestCamera()).rejects.toMatchObject({ kind: 'permission-denied' })
    expect(toCameraFailure('permission-denied').code).toBe('CAMERA_PERMISSION_DENIED')
  })

  it('NotReadableError（设备被占用）→ device-busy，而不是权限问题', async () => {
    const devices = makeMediaDevicesStub()
    devices.cameraFailWith = namedError('NotReadableError')
    installMediaDevices(devices)

    await expect(requestCamera()).rejects.toMatchObject({ kind: 'device-busy' })
    // 归到"权限"会让界面叫学生去点权限弹窗，而他点一百次也不会成功。
    expect(toCameraFailureKind(namedError('NotReadableError'))).toBe('device-busy')
  })

  it('没有设备 / 约束不满足 → device-missing', () => {
    expect(toCameraFailureKind(namedError('NotFoundError'))).toBe('device-missing')
    expect(toCameraFailureKind(namedError('OverconstrainedError'))).toBe('device-missing')
  })

  it('不认识的错误 → unsupported（宁可说"换浏览器"，也不让它悄悄通过）', () => {
    expect(toCameraFailureKind(new Error('boom'))).toBe('unsupported')
    expect(toCameraFailureKind(undefined)).toBe('unsupported')
  })

  it('拿到了流却没有 video track → device-missing，并且把轨道全部停掉', async () => {
    const track = makeFakeCameraTrack()
    // "有流但没有 video 轨道"：某些实现（虚拟摄像头、被系统降级）会这样返回。
    installMediaDevices({
      getUserMedia: () =>
        Promise.resolve({ active: true, getTracks: () => [track], getVideoTracks: () => [] }),
    })

    await expect(requestCamera()).rejects.toMatchObject({ kind: 'device-missing' })
    // 半成品流同样必须释放：漏掉一条就等于"开启失败"的提示下摄像头灯还亮着。
    expect(track.stopCalls).toBe(1)
  })

  it('请求失败时也会尽力停掉浏览器可能已经建好的流', async () => {
    const leaked = makeFakeCameraTrack()
    /**
     * 真实实现里"先建流再报错"并非不可能，`stopEverything` 检查的正是异常对象上
     * 有没有 `getTracks`——所以这里把流挂在异常对象上（而不是另造一个参数）。
     */
    const error = Object.assign(namedError('NotReadableError'), {
      getTracks: () => [leaked],
    })
    installMediaDevices({ getUserMedia: () => Promise.reject(error) })

    await expect(requestCamera()).rejects.toBeInstanceOf(CameraCaptureError)
    expect(leaked.stopCalls).toBe(1)
  })
})

describe('轨道生命周期（§24 的"开 / 关 / 再开"）', () => {
  it('stop() 幂等，并且真的把设备交还（指示灯必须灭）', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)
    const capture = await requestCamera()
    const track = devices.cameraStreams[0]?.getVideoTracks()[0]

    capture.stop()
    capture.stop()
    releaseCameraCapture(capture)

    expect(track?.stopCalls).toBe(1)
    expect(track?.readyState).toBe('ended')
  })

  it('releaseCameraCapture 对 null / undefined 安全（退出路径不该先判空）', () => {
    expect(() => releaseCameraCapture(null)).not.toThrow()
    expect(() => releaseCameraCapture(undefined)).not.toThrow()
  })

  it('设备被拔出（track ended）→ 通知订阅者，并同步释放', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)
    const capture = await requestCamera()
    const track = devices.cameraStreams[0]?.getVideoTracks()[0]
    let ended = 0
    capture.onEnded(() => {
      ended += 1
    })

    track?.emitEnded()

    expect(ended).toBe(1)
    expect(track?.stopCalls).toBe(1)
  })

  it('订阅时轨道就已经结束 → 立刻回调一次（否则界面会停在"已开启"）', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)
    const capture = await requestCamera()
    devices.cameraStreams[0]?.getVideoTracks()[0]?.stop()

    let ended = 0
    const off = capture.onEnded(() => {
      ended += 1
    })
    off()

    expect(ended).toBe(1)
  })
})

describe('失败文案（§58 / §80：必须给出下一步，且明说不影响上课）', () => {
  it('权限被拒：说明"仍然可以正常上课"并给出恢复路径', () => {
    const message = describeCameraFailure('permission-denied')
    expect(message).toContain('没有授予摄像头权限')
    expect(message).toContain('仍然可以正常上课')
  })

  it('设备被占用：指向"其他程序占用"这个真实成因', () => {
    const message = describeCameraFailure('device-busy')
    expect(message).toContain('占用')
    expect(message).toContain('不会影响你上课')
  })

  it('没有码的类别明确留空，而不是借一个语义不符的 §58 错误码', () => {
    expect(toCameraFailure('device-busy').code).toBeNull()
    expect(toCameraFailure('unsupported').code).toBeNull()
    expect(toCameraFailure('track-ended').code).toBeNull()
  })

  it('CameraCaptureError 会把它自己的类别透传给调用方', () => {
    expect(toCameraFailureKindOf(new CameraCaptureError('device-busy'))).toBe('device-busy')
    expect(toCameraFailureKindOf(namedError('NotAllowedError'))).toBe('permission-denied')
  })
})
