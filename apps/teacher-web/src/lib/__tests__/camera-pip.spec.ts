import { describe, expect, it } from 'vitest'
import {
  CAMERA_PIP_STATES,
  CAMERA_PIP_TEXT,
  deriveCameraPipState,
  isCameraPipVisible,
  shouldSubscribeCamera,
} from '../camera-pip.ts'
import {
  makeLeftStudent,
  makeMonitorStudent,
  makeNotJoinedStudent,
} from '../../__tests__/monitor-fixtures.ts'

/**
 * 画中画展示状态测试（§24 / §29 / §30）。
 *
 * 这一层是"业务状态 + 媒体状态"的合成点（§51），所以每条用例都在问同一个问题：
 * **老师屏幕上那个小窗该不该出现，以及不出现时是哪一种"没有"。**
 *
 * 最重要的一条是反面断言：`waiting` / `failed` / `off` 三种情况**都不显示**。
 * 画一个空框会让老师以为"学生把摄像头关了"，而事实可能是老师自己这边没订上。
 */

describe('画中画状态合成（§24 / §29）', () => {
  it('未开启摄像头 → off（DTO 说没开就是没开，媒体层说什么都不算）', () => {
    const student = makeMonitorStudent({ camera: { active: false } })

    const state = deriveCameraPipState(student, { state: 'subscribed', hasSubscription: true })

    expect(state).toBe('off')
    expect(isCameraPipVisible(state)).toBe(false)
  })

  it('开着且订阅到位 → visible', () => {
    const student = makeMonitorStudent({ camera: { active: true } })

    const state = deriveCameraPipState(student, { state: 'subscribed', hasSubscription: true })

    expect(state).toBe('visible')
    expect(isCameraPipVisible(state)).toBe(true)
  })

  it('开着但订阅还没到 → waiting（不画空窗）', () => {
    const student = makeMonitorStudent({ camera: { active: true } })

    expect(deriveCameraPipState(student, { state: 'pending', hasSubscription: false })).toBe(
      'waiting',
    )
    expect(deriveCameraPipState(student, { state: 'none', hasSubscription: false })).toBe('waiting')
  })

  it('订阅失败 → failed（与 waiting 分开：一个还会来，一个不会了）', () => {
    const student = makeMonitorStudent({ camera: { active: true } })

    expect(deriveCameraPipState(student, { state: 'failed', hasSubscription: false })).toBe(
      'failed',
    )
  })

  it('学生已经不在课堂里 → off（哪怕 DTO 里 camera 还留着 true）', () => {
    // 快照没刷新时可能出现这种组合：摄像头字段没清，但人已经离开了。
    const student = makeLeftStudent({ camera: { active: true } })

    expect(deriveCameraPipState(student, { state: 'subscribed', hasSubscription: true })).toBe(
      'off',
    )
  })

  it('未进入课堂的学生同理：没有会话就没有摄像头画面可言', () => {
    const student = makeNotJoinedStudent({ camera: { active: true } })

    expect(deriveCameraPipState(student, { state: 'none', hasSubscription: false })).toBe('off')
  })

  it('只有 visible 会让组件渲染 <video>（四种状态都有明确的去处）', () => {
    const visible = CAMERA_PIP_STATES.filter(isCameraPipVisible)
    expect(visible).toEqual(['visible'])
  })

  it('Focus 面板的三种"没有画面"各有一句话，且互不相同', () => {
    const texts = Object.values(CAMERA_PIP_TEXT)
    expect(texts).toHaveLength(3)
    expect(new Set(texts).size).toBe(3)
    expect(CAMERA_PIP_TEXT.off).toContain('未开启')
    expect(CAMERA_PIP_TEXT.waiting).toContain('正在获取')
    expect(CAMERA_PIP_TEXT.failed).toContain('失败')
  })
})

describe('摄像头订阅的判据（§52 的业务侧）', () => {
  it('在线 + 摄像头开着 → 值得订阅', () => {
    expect(shouldSubscribeCamera(makeMonitorStudent({ camera: { active: true } }))).toBe(true)
  })

  it('摄像头没开 → 不订阅（哪怕屏幕正在发布）', () => {
    expect(
      shouldSubscribeCamera(
        makeMonitorStudent({ screen: { active: true }, camera: { active: false } }),
      ),
    ).toBe(false)
  })

  it('屏幕中断不影响摄像头：两条轨道互不派生（§21/§24）', () => {
    expect(
      shouldSubscribeCamera(
        makeMonitorStudent({
          sessionStatus: 'SCREEN_LOST',
          screen: { active: false },
          camera: { active: true },
        }),
      ),
    ).toBe(true)
  })

  it('没进入 / 已离开的学生一律不订（不留下行）', () => {
    expect(shouldSubscribeCamera(makeNotJoinedStudent({ camera: { active: true } }))).toBe(false)
    expect(shouldSubscribeCamera(makeLeftStudent({ camera: { active: true } }))).toBe(false)
  })
})
