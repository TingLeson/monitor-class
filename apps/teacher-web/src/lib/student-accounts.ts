import { CLASSROOM_STUDENTS_ADD_MAX } from '@classwatch/shared-types'

/** 解析结果。 */
export interface ParsedStudentAccounts {
  /** 去重后的账号，保持老师输入的顺序。 */
  accounts: string[]
  /** 因为超过单次上限而被丢弃的账号数（界面要告诉老师"还有几个没提交"）。 */
  dropped: number
}

/**
 * 把老师粘贴的一整段文本拆成账号数组（§11；teacher.md §4.1）。
 *
 * 分隔符同时接受换行、中英文逗号、分号、顿号、制表符与空格：老师手上的名单可能来自
 * Excel 一列（换行）、聊天记录（逗号）或直接从表格里复制（制表符），
 * 要求他们先整理格式是最没有必要的摩擦。
 *
 * WHY 只做"去空白 + 去重 + 截断"，不在这里校验账号格式：
 * 账号格式规则属于后端（§63 input validation），前端猜一套规则只有两种结果——
 * 拦掉了合法账号，或者放过非法账号再让后端逐条拒绝（而后者已经能逐条说明原因）。
 * 唯一在本地处理的是"空字符串"：它一定是分隔符产生的噪声，不是老师的输入意图。
 */
export function parseStudentAccounts(
  input: string,
  limit: number = CLASSROOM_STUDENTS_ADD_MAX,
): ParsedStudentAccounts {
  const seen = new Set<string>()
  for (const raw of input.split(/[\s,，、;；]+/)) {
    const account = raw.trim()
    // Set 既去重也保持插入顺序：同一批里重复写两次账号不该变成两行 rejected。
    if (account) seen.add(account)
  }

  const all = [...seen]
  const accounts = all.slice(0, Math.max(0, limit))
  return { accounts, dropped: all.length - accounts.length }
}

/** 描述一次批量添加的结果（成功数 + 被拒数），用于结果提示的第一行。 */
export function describeAddResult(addedCount: number, rejectedCount: number): string {
  if (rejectedCount === 0) return `成功加入 ${addedCount} 个学生`
  if (addedCount === 0) return `没有账号被加入，${rejectedCount} 个账号被拒绝`
  return `成功加入 ${addedCount} 个学生，${rejectedCount} 个账号被拒绝`
}
