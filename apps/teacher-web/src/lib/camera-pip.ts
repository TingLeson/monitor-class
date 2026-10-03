import type { MonitorStudent } from '@classwatch/shared-types'
import { isStudentEntered, type MonitorMediaState } from './monitor-status.ts'

/**
 * 摄像头画中画（§24 / §29 / §30 / §52 / §75）。
 *
 * 与 `monitor-status.ts` 是同一种东西：**展示层词表**，把"业务状态（Monitor DTO）
 * + 媒体状态（LiveKit 订阅）"合成一个可以直接渲染的结论，只此一处
 * （§51 的合成只做一次；组件里散落 if 判断是最容易让两个界面说出两套话的写法）。
 *
 * 两条纪律：
 *
 * 1. **业务面以 DTO 为准**。`camera.active` 是后端观测到的事实（§24 的 webhook），
 *    前端绝不用"房间里有没有 camera 轨道"去反推——那正是 §51 禁止的
 *    "拿 LiveKit Participant 当业务模型"。轨道可能晚到几百毫秒，也可能因为老师端
 *    网络问题一直订不上；这两种情况要显示成不同的东西。
 * 2. **没有画面就不画小窗**。§29 的画中画是一个"有/无"的东西：'waiting' 与
 *    'failed' 都**不渲染** `<video>`（空白小窗会被老师读成"摄像头坏了"），
 *    但 Focus 面板会把原因写出来——那里有位置说清一句话，网格卡片没有。
 */

/**
 * 画中画的展示状态。
 *
 * - `off`：学生没开摄像头（或者人已经不在课堂里）→ 不显示；
 * - `waiting`：后端说他开着，媒体还没到 → 不显示（不给空窗）；
 * - `visible`：订阅到位，可以画；
 * - `failed`：订阅失败 → 不显示，Focus 面板给一句原因。
 *
 * `waiting` 与 `failed` 必须分开：前者是"再等一会儿就会有"，后者是"这一轮不会有"，
 * 合成一个值之后，排障时再也说不清那次没有画面到底是哪一类。
 */
export const CAMERA_PIP_STATES = ['off', 'waiting', 'visible', 'failed'] as const

export type CameraPipState = (typeof CAMERA_PIP_STATES)[number]

/**
 * 这个学生的摄像头画面是不是**现在**就有。
 *
 * 只有它等于 `'visible'` 时组件才会渲染 `<video>`：其余三种都表示"没有可播的东西"，
 * 而画一个空框是最糟的处理方式（老师无法区分"摄像头坏了"与"学生没开"）。
 */
export function isCameraPipVisible(state: CameraPipState): boolean {
  return state === 'visible'
}

/**
 * 这张卡片是否**值得**订阅摄像头轨道（§52 的业务侧判据；可见性由 store 再叠一层）。
 *
 * 与 `shouldSubscribeScreen` 同构，但**判的是另一个字段**：`camera.active`。
 * §21/§24 明确屏幕是 mandatory、摄像头是 optional，两者互不派生——
 * "屏幕在线"不代表"摄像头开着"，反之亦然。
 */
export function shouldSubscribeCamera(student: MonitorStudent): boolean {
  // 没进课堂（未进入 / 已断开 / 已离开）的人不该留下任何下行。
  if (!isStudentEntered(student)) return false
  return student.camera.active
}

/**
 * 合成画中画状态。
 *
 * 判定顺序即优先级，两条容易写错的边界：
 * 1. **`camera.active === false` 优先于一切**：DTO 说没开就是没开，哪怕本地还挂着
 *    一条上一秒的订阅（学生刚点关闭）——界面必须立刻收起小窗，而不是等轨道消失。
 * 2. **人不在课堂里也归到 `off`**：`STUDENT_OFFLINE` 会把 `camera.active` 一起置假，
 *    但如果只来了 `DISCONNECTED` 而快照还没刷新，这里也不能继续画一个已经没人维护的画面。
 */
export function deriveCameraPipState(
  student: MonitorStudent,
  media: { state: MonitorMediaState; hasSubscription: boolean },
): CameraPipState {
  if (!student.camera.active) return 'off'
  if (!isStudentEntered(student)) return 'off'
  if (media.state === 'failed') return 'failed'
  if (media.hasSubscription) return 'visible'
  return 'waiting'
}

/**
 * Focus 面板 Camera 区在没有画面时的说明（§30）。
 *
 * `visible` 没有文案：那一档渲染的是画面本身。措辞与网格卡片上的"摄像头已开启"
 * 分工明确——卡片回答"他开没开"，这里回答"我能不能看到"。
 */
export const CAMERA_PIP_TEXT: Record<Exclude<CameraPipState, 'visible'>, string> = {
  off: '学生未开启摄像头',
  waiting: '正在获取摄像头画面…',
  failed: '摄像头画面订阅失败',
}
