/**
 * 学生端私密语音状态机（§25 / §31 / §76）。
 *
 * 老师在服务端只有一个目标（§31），而**学生这一侧**要回答的是两个独立的问题：
 *
 * ```text
 * 1. 老师现在是不是在对我讲话？        → teacherDisplayName 有值
 * 2. 我是不是还没决定要不要开麦？      → requestPending
 * ```
 *
 * 把它们塞进一个布尔会产生两个真实的错误结论：
 * - "老师正在讲话" 与 "老师请求我开麦" 被合成一件事之后，学生点了「暂不开启」
 *   就会以为整段沟通结束了——而 §25 说得很清楚：**老师仍然可以单向讲话**；
 * - 反过来，"我拒绝过"这个事实必须留住：否则后到的 `PRIVATE_TALK_STARTED`
 *   会把同一个提示又弹一次，学生每拒绝一次就要再拒绝一次。
 *
 * 因此状态是三个字段，迁移只有下面这几个纯函数。放在纯函数里而不是模板里：
 * 事件的到达顺序（REQUEST 先于 STARTED、还是相反）在真实网络里不确定，
 * 而这几个函数的组合必须对两种顺序都给出同一个结果——这件事只有单元测试能钉住。
 */

export interface StudentPrivateTalk {
  /** 正在对我讲话的老师姓名（服务端在事件里给的是显示名，学生端没有别的身份信息，§26）。 */
  teacherDisplayName: string
  /** 是否还需要问学生"要不要开麦"。 */
  requestPending: boolean
  /**
   * 学生已经明确点过「暂不开启」。
   *
   * 留住它的唯一目的是**不重复打扰**：同一个老师在同一次沟通里再发一次
   * `PRIVATE_TALK_STARTED`，不该让提示重新出现。
   */
  declined: boolean
}

/**
 * 收到 `PRIVATE_TALK_REQUEST`（只在学生麦克风未开启时服务端才发，§25）。
 *
 * 返回的 `requestPending` 仍然由**本地**麦克风状态决定：服务端的判断可能比
 * 浏览器里那几百毫秒的连接状态新，而"要不要问"这件事的本机事实更权威
 * （刚刚点过开麦的学生不该再被问一次）。
 */
export function applyTalkRequest(
  current: StudentPrivateTalk | null,
  teacherDisplayName: string,
  microphoneOn: boolean,
): StudentPrivateTalk {
  const sameTalk = current !== null && current.teacherDisplayName === teacherDisplayName
  const declined = sameTalk && current.declined
  return {
    teacherDisplayName,
    requestPending: !microphoneOn && !declined,
    declined,
  }
}

/**
 * 收到 `PRIVATE_TALK_STARTED`（§31：老师选中了我）。
 *
 * 它**不覆盖** `declined`，也不把已经没人问的提示重新打开：
 * - 学生已经开麦 → 没有可问的（requestPending = false）；
 * - 学生拒绝过 → 也不再问（declined 仍然是 true）；
 * - 其余情况 → 提示应该在场（老师在讲话，而学生还没决定）。
 */
export function applyTalkStarted(
  current: StudentPrivateTalk | null,
  teacherDisplayName: string,
  microphoneOn: boolean,
): StudentPrivateTalk {
  const sameTalk = current !== null && current.teacherDisplayName === teacherDisplayName
  const declined = sameTalk && current.declined
  return {
    teacherDisplayName,
    requestPending: !microphoneOn && !declined,
    declined,
  }
}

/** 学生点了「暂不开启」（§25）：关掉提示，但沟通本身继续（老师仍能单向讲话）。 */
export function declineTalkRequest(current: StudentPrivateTalk | null): StudentPrivateTalk | null {
  if (current === null) return null
  return { ...current, requestPending: false, declined: true }
}

/**
 * 学生的麦克风真的开启了。
 *
 * WHY 由本地动作直接收掉提示，而不是等服务端再发一条事件：学生点「开启麦克风」是
 * 一次明确的选择，界面必须立刻反映它。§25 要求"开启成功后提示会变成
 * 老师正在与你语音沟通"——那句话由 `teacherDisplayName` 已经在场保证，
 * 这里只需要把"还在等你决定"这一半去掉。
 */
export function markMicrophoneEnabled(
  current: StudentPrivateTalk | null,
): StudentPrivateTalk | null {
  if (current === null || !current.requestPending) return current
  return { ...current, requestPending: false }
}

/**
 * 收到 `PRIVATE_TALK_ENDED`：整段沟通结束（§31 的 TALKING → IDLE）。
 *
 * 连带清掉 `declined`：下一次老师再找我，是一个**新的**请求，必须重新问。
 */
export function endPrivateTalk(): null {
  return null
}

/**
 * 是否有一段老师与我的语音沟通正在进行（界面据此显示持续指示）。
 *
 * WHY 不看 `requestPending`：§25 的产品事实是"老师选中了我"就已经在讲话了
 * （老师可以向未开麦的学生单向讲话）。提示卡片问的是"你要不要也让他听到你"，
 * 那是**同一段沟通里的另一个问题**，不是"沟通还没开始"。
 * 把两者混在一起，学生点了「暂不开启」之后整个界面就会安静下来，
 * 而老师的下行其实还在——那正是最伤信任的一种沉默。
 */
export function hasPrivateTalk(state: StudentPrivateTalk | null): boolean {
  return state !== null
}

/** 「X 希望与你进行语音沟通。」（§25 的原话，逐字）。 */
export function privateTalkRequestTitle(state: StudentPrivateTalk): string {
  return `${state.teacherDisplayName}希望与你进行语音沟通。`
}

/** 「正在与 X 语音沟通」（§25 的持续指示）。 */
export function privateTalkActiveText(state: StudentPrivateTalk): string {
  return `正在与${state.teacherDisplayName}语音沟通`
}

/**
 * 点「暂不开启」之后必须说出来的一句话（§25）。
 *
 * 这不是客套：学生点"不"的时候最可能的心智模型是"整段沟通结束了"，
 * 而事实上老师仍然能对他单向讲话。界面如果不说明，学生会在听到老师声音时
 * 以为系统在偷偷开麦——那正是最伤信任的一种误解。
 */
export const PRIVATE_TALK_DECLINE_NOTE = '你仍然能听到老师讲话，只是老师听不到你。'
