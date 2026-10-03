import { describe, expect, it } from 'vitest'
import {
  applyMicrophoneMute,
  checkMicrophoneSupport,
  MIC_CONSTRAINTS,
  MicrophoneCaptureError,
  releaseMicrophoneCapture,
  requestMicrophone,
  toMicFailure,
  toMicFailureKind,
  toMicFailureKindOf,
} from '../mic-capture'
import { installMediaDevices, makeMediaDevicesStub } from '../../__tests__/screen-fixtures'

/**
 * 麦克风采集层测试（§25 / §58 / §76）。
 *
 * 与 `camera-capture.spec.ts` 同一套盯法，但重点不同：
 *
 * 1. **约束只有 audio**：摄像头有它自己的开关（§24），麦克风这条路上出现 `video`
 *    就是超范围收集；
 * 2. **能力自检在请求之前**：没有 `getUserMedia` 时绝不"试一下再说"；
 * 3. **失败不留活轨道**：一条活着的 audio track 意味着系统录音指示灯一直亮着；
 * 4. **静音 ≠ 关麦**（§31）：`track.enabled` 才是"静音自己"，`stop()` 是"关麦"。
 */

/** 造一个带指定 error.name 的异常（浏览器抛的就是这种形状）。 */
function namedError(name: string): Error {
  const error = new Error(name)
  error.name = name
  return error
}

describe('麦克风能力自检（§17 的麦克风对应物）', () => {
  it('没有 navigator.mediaDevices → 不支持，原因为 no-mediaDevices', () => {
    expect(checkMicrophoneSupport()).toEqual({ supported: false, reason: 'no-mediaDevices' })
  })

  it('有 mediaDevices 但没有 getUserMedia → 不支持（不能"试一下再说"）', () => {
    installMediaDevices({ getDisplayMedia: () => Promise.resolve(null) })

    expect(checkMicrophoneSupport()).toEqual({ supported: false, reason: 'no-getUserMedia' })
  })

  it('支持时不做任何请求（纯读取检查，绝无副作用）', () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)

    expect(checkMicrophoneSupport()).toEqual({ supported: true })
    expect(devices.micCalls).toHaveLength(0)
  })
})

describe('请求麦克风（§25）', () => {
  it('用 §25 冻结的 {audio: true} 约束请求（不含 video，摄像头有自己的开关）', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)

    const capture = await requestMicrophone()

    expect(devices.micCalls).toHaveLength(1)
    expect(devices.micCalls[0]?.constraints).toEqual({ audio: true })
    expect(MIC_CONSTRAINTS).toEqual({ audio: true })
    // 拿到的就是替身造出来的那条轨道（不是重新采集的一条）。
    expect(capture.track).toBe(devices.micStreams[0]?.getAudioTracks()[0])
    // 摄像头那条路径一次都没被碰过：两条约束不会互相夹带。
    expect(devices.cameraCalls).toHaveLength(0)
  })

  it('不支持时直接拒绝，且**不**调用 getUserMedia', async () => {
    installMediaDevices({ getDisplayMedia: () => Promise.resolve(null) })

    await expect(requestMicrophone()).rejects.toBeInstanceOf(MicrophoneCaptureError)
    await expect(requestMicrophone()).rejects.toMatchObject({ kind: 'unsupported' })
  })

  it('NotAllowedError（学生拒绝 / 系统未授权）→ permission-denied，且带 §58 的码', async () => {
    const devices = makeMediaDevicesStub()
    devices.micFailWith = namedError('NotAllowedError')
    installMediaDevices(devices)

    const failure = await requestMicrophone().catch((cause: unknown) => cause)

    expect(failure).toBeInstanceOf(MicrophoneCaptureError)
    expect(toMicFailureKindOf(failure)).toBe('permission-denied')
    expect(toMicFailure('permission-denied').code).toBe('MIC_PERMISSION_DENIED')
    // 文案必须给出下一步动作，并且明说不会影响上课（§21）。
    expect(toMicFailure('permission-denied').message).toContain('权限')
    expect(toMicFailure('permission-denied').message).toContain('仍然可以正常上课')
  })

  it('NotReadableError（设备被别的程序占用）→ device-busy，不是权限问题', () => {
    expect(toMicFailureKind(namedError('NotReadableError'))).toBe('device-busy')
    expect(toMicFailureKind(namedError('AbortError'))).toBe('device-busy')
    expect(toMicFailure('device-busy').code).toBeNull()
    expect(toMicFailure('device-busy').message).toContain('占用')
  })

  it('NotFoundError → device-missing；不认识的错误名降级成 unsupported', () => {
    expect(toMicFailureKind(namedError('NotFoundError'))).toBe('device-missing')
    expect(toMicFailureKind(namedError('OverconstrainedError'))).toBe('device-missing')
    expect(toMicFailureKind(namedError('SomethingNewError'))).toBe('unsupported')
  })

  it('释放采集是幂等的，并且**真的** stop 掉轨道（系统指示灯必须灭）', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)
    const capture = await requestMicrophone()

    releaseMicrophoneCapture(capture)
    releaseMicrophoneCapture(capture)
    releaseMicrophoneCapture(null)

    const track = devices.micStreams[0]?.getAudioTracks()[0]
    expect(track?.stopCalls).toBe(1)
    expect(track?.readyState).toBe('ended')
  })

  it('轨道结束会通知订阅者，并且顺带释放（设备没了就不能再当成"已开启"）', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)
    const capture = await requestMicrophone()
    let ended = 0
    capture.onEnded(() => {
      ended += 1
    })

    devices.micStreams[0]?.getAudioTracks()[0]?.emitEnded()

    expect(ended).toBe(1)
    expect(devices.micStreams[0]?.getAudioTracks()[0]?.readyState).toBe('ended')
  })
})

describe('本地静音（§31 的"静音自己"）', () => {
  it('静音只改 track.enabled，不停轨道（老师那端的订阅不会被拆掉）', async () => {
    const devices = makeMediaDevicesStub()
    installMediaDevices(devices)
    const capture = await requestMicrophone()
    const track = devices.micStreams[0]?.getAudioTracks()[0]

    expect(applyMicrophoneMute(capture, true)).toBe(true)

    expect(track?.enabled).toBe(false)
    expect(track?.readyState).toBe('live')
    expect(track?.stopCalls).toBe(0)

    expect(applyMicrophoneMute(capture, false)).toBe(true)
    expect(track?.enabled).toBe(true)
  })

  it('没有采集时是空操作（界面本来也不会显示静音按钮）', () => {
    expect(applyMicrophoneMute(null, true)).toBe(false)
  })
})
