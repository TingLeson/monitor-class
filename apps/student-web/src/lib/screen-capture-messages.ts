import {
  surfaceLabel,
  type ScreenCaptureErrorCode,
  type ScreenCaptureUnsupportedReason,
  type ScreenSurface,
} from './screen-capture'

/**
 * 屏幕捕获失败 → 可直接展示给学生的中文文案（§16 / §17 / §22 / §58）。
 *
 * WHY 单独一个文件、而不是把文案写进 screen-capture.ts：
 * `screen-capture.ts` 描述的是**浏览器行为**（约束、Gate、事件），文案描述的是
 * **学生该怎么做**。两者的修改动因完全不同——调整措辞（学生反馈"看不懂"）不该
 * 触碰 Gate 代码，改 Gate 也不该顺手改掉一句已经在上线文案里定稿的话。
 *
 * 三条硬约束：
 * 1. §16 / §17 的原文**逐字**出现（"当前浏览器无法确认你是否共享了完整显示器。
 *    请使用系统支持的最新版 Chrome 或 Edge。"），不得改写、不得省略；
 * 2. §58：不显示原始报文、不显示错误码、不显示英文技术信息；
 * 3. 每句话都必须给出**下一步动作**（重新选择 / 去系统设置 / 换浏览器 / 重新共享）。
 *    只说"失败了"会把学生卡在 PreJoin 页上，而他唯一能做的就是反复点同一个按钮。
 */

/** §17 / §16 的原文，逐字。两处出现（能力不足、无法确认共享面）必须是同一句。 */
export const SCREEN_UNVERIFIABLE_NOTICE =
  '当前浏览器无法确认你是否共享了完整显示器。请使用系统支持的最新版 Chrome 或 Edge。'

/**
 * 能力自检失败（§17）的具体说明。
 *
 * 三种原因给的是同一句结论（都要换浏览器），但**行内**多一句技术说明：学生（或
 * 帮他排查的老师）看到"缺少 getDisplayMedia"就能判断这是浏览器太旧，而不是
 * "系统坏了"。措辞刻意保持为可读的中文，不是堆栈。
 */
const UNSUPPORTED_REASONS: Record<ScreenCaptureUnsupportedReason, string> = {
  /**
   * 最容易被误判的一种：**换浏览器解决不了**，所以要给出地址栏层面的动作。
   * Chrome / Edge / Safari 都只在 https:// 或 localhost 下提供屏幕捕获——
   * 用 http:// + 局域网 IP 打开时，浏览器连 API 都不暴露。这是浏览器的安全模型，
   * 不是浏览器版本问题，也不是本系统的限制。
   */
  insecureContext:
    '当前地址不是安全上下文：浏览器只在 https:// 或 localhost 下提供屏幕捕获接口，' +
    '而你现在用的是 http:// 的局域网地址。请让老师把入口换成 https://，或在运行服务的那台电脑上用 localhost 打开。',
  'no-mediaDevices': '这个浏览器没有提供任何媒体设备接口（navigator.mediaDevices 不存在）。',
  'no-getDisplayMedia': '这个浏览器没有屏幕共享接口（getDisplayMedia 不存在）。',
  'no-getSettings':
    '这个浏览器无法报告屏幕共享的范围（MediaStreamTrack.getSettings 不存在），因此无法确认你共享的是整块显示器。',
}

/** 所有可能出现的情境。用可辨识联合，避免"用可选字段猜是哪种失败"。 */
export type ScreenGateFailure =
  /** §17 能力自检失败：请求还没发生。 */
  | { kind: 'unsupported'; reason: ScreenCaptureUnsupportedReason }
  /** §16 Gate 拒绝 / 请求本身失败。 */
  | {
      kind: 'capture'
      code: ScreenCaptureErrorCode
      surface?: ScreenSurface | null
      causeName?: string | null
    }

/** 每一句文案的构成：一句结论 + 一句下一步动作。 */
export interface ScreenGateMessage {
  /** 加粗标题行。 */
  title: string
  /** 正文：原因 + 下一步动作。 */
  description: string
}

/**
 * 把失败翻译成学生看得懂的一段话。
 *
 * 分支顺序即优先级：`causeName` 先于 `code`，因为同一个
 * `SCREEN_PERMISSION_DENIED` 之下，"你取消了"和"macOS 没给屏幕录制权限"需要
 * 完全不同的补救动作，而只有 causeName 能区分它们（`ScreenGateError.causeName`）。
 */
export function describeScreenGateFailure(failure: ScreenGateFailure): ScreenGateMessage {
  if (failure.kind === 'unsupported') {
    return {
      title: '当前浏览器无法共享整个屏幕',
      description: `${SCREEN_UNVERIFIABLE_NOTICE}（${UNSUPPORTED_REASONS[failure.reason]}）`,
    }
  }

  const { code, causeName } = failure
  const surface = failure.surface ?? null

  switch (code) {
    /**
     * §16 原文那段话在这里出现：API 在、证书也在，但 `displaySurface` 缺失/为空，
     * 我们**无法确认**共享范围。它和"确认不是整屏"是两种不同的坏消息，
     * 因此不能共用 SCREEN_NOT_MONITOR 的措辞（那句会让学生去重选，而重选没有用）。
     *
     * 三个分支对应三种真实成因，行内说明不同但结论一致：
     * - 请求前就发现接口缺失（causeName 是自检原因）；
     * - Gate 读到 `displaySurface` 缺失 —— surface 为 'unknown' 且 causeName 是 'displaySurface'；
     * - 调用本身抛了别的错（= 这个浏览器的屏幕捕获不可用）。
     */
    case 'SCREEN_API_UNSUPPORTED': {
      if (causeName === 'no-getDisplayMedia' || causeName === 'no-mediaDevices') {
        return { title: '当前浏览器无法共享整个屏幕', description: SCREEN_UNVERIFIABLE_NOTICE }
      }
      if (causeName === 'displaySurface') {
        return {
          title: '无法确认你共享的是完整显示器',
          description: `${SCREEN_UNVERIFIABLE_NOTICE}请改用最新版 Chrome 或 Edge，并在选择窗口里选择「整个屏幕」。`,
        }
      }
      return {
        title: '这台设备无法开始屏幕共享',
        description: `${SCREEN_UNVERIFIABLE_NOTICE}如果重试后仍然失败，请重启浏览器或改用系统支持的最新版 Chrome / Edge。`,
      }
    }

    /**
     * §65 Case 6/7：学生选了 Chrome 标签页或某个应用窗口。
     *
     * 必须说清"你选的是窗口/标签页"，并且明确要求重选"整个屏幕"——只说"请共享
     * 整个屏幕"会让学生以为上次点的就是整个屏幕，然后在同一个选择器里重复同一个选择。
     */
    case 'SCREEN_NOT_MONITOR':
      return {
        title: `不能共享${surfaceLabel(surface)}`,
        description: `你刚才选择的是${surfaceLabel(
          surface,
        )}，本课堂要求共享整块显示器。请重新点击下面的按钮，在浏览器的选择窗口里选择「整个屏幕」，不要选择窗口或浏览器标签页。`,
      }

    case 'SCREEN_TRACK_ENDED':
      return {
        title: '屏幕共享已停止',
        description:
          '屏幕共享已经结束，当前课堂要求持续共享整个屏幕。请重新点击下面的按钮共享整个屏幕后才能继续上课。',
      }

    case 'SCREEN_PERMISSION_DENIED':
      /**
       * `NotReadableError`：授权通过了，但系统层面起不了捕获。
       *
       * 实测（本机 Chrome 154 / macOS）最常见的成因就是系统没有授予浏览器
       * "屏幕录制"权限。**绝不能**报成"你选错了共享面"——那会让学生反复重选
       * 「整个屏幕」却永远失败，而真正的开关在系统设置里。
       */
      if (causeName === 'NotReadableError') {
        return {
          title: '无法开始屏幕捕获',
          description:
            '浏览器没有拿到屏幕画面。请检查系统是否允许浏览器录制屏幕（macOS：「系统设置 → 隐私与安全性 → 屏幕录制」，Windows：「设置 → 隐私 → 屏幕截图」），授权后需要完全退出并重新打开浏览器。然后再点一次下面的按钮。',
        }
      }
      return {
        title: '未获得屏幕共享权限',
        description:
          '你取消了共享，或者浏览器没有允许本次共享。进入课堂必须共享整个屏幕。请重新点击下面的按钮，并在浏览器的提示里选择「整个屏幕」后确认共享。',
      }
  }
}

/** Gate 相关状态的最小快照（PreJoin 与会话页各有一份，形状相同）。 */
export interface ScreenGateStateInput {
  /** 曾经共享过、现在断了（§22 → 要提示重新共享）。 */
  isLost: boolean
  /** 能力自检失败的原因（§17）。 */
  unsupportedReason: ScreenCaptureUnsupportedReason | null
  /** Gate 拒绝 / 请求失败。 */
  failure: {
    code: ScreenCaptureErrorCode
    surface: ScreenSurface | null
    causeName: string | null
  } | null
}

/**
 * 把 screen-share store 的三类状态折叠成一段可直接渲染的说明（§16/§17/§22）。
 *
 * WHY 抽出来给两个页面共用：PreJoin（第一次共享）与会话页（屏幕丢失后重新共享，
 * §22）必须给出**完全一样**的话术。学生在这两个地方遇到的是同一件事——"现在没有
 * 满足课堂要求的共享"——如果措辞不同，他会以为遇到了两种不同的问题。
 *
 * 返回 null 表示"没有什么要说的"（正在共享 / 未开始）。
 */
export function describeScreenGateState(state: ScreenGateStateInput): ScreenGateMessage | null {
  if (state.isLost) {
    return describeScreenGateFailure({ kind: 'capture', code: 'SCREEN_TRACK_ENDED' })
  }
  if (state.unsupportedReason !== null) {
    return describeScreenGateFailure({ kind: 'unsupported', reason: state.unsupportedReason })
  }
  if (state.failure !== null) {
    return describeScreenGateFailure({
      kind: 'capture',
      code: state.failure.code,
      surface: state.failure.surface,
      causeName: state.failure.causeName,
    })
  }
  return null
}
