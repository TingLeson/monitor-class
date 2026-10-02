import { describe, expect, it } from 'vitest'
import { describeAddResult, parseStudentAccounts } from '../student-accounts'

/**
 * 粘贴式账号解析（§11；docs/frontend/teacher.md §4.1）。
 *
 * 这一层决定了"老师从 Excel / 聊天记录里直接粘贴"能不能用，以及会不会把
 * 空字符串或重复账号塞进请求（空字符串会被后端记成 INVALID_REQUEST，
 * 让一次本来全对的提交出现一行莫名其妙的拒绝）。
 */
describe('parseStudentAccounts', () => {
  it('按换行、逗号、空格、制表符拆分账号', () => {
    const { accounts } = parseStudentAccounts('S10086\nS10087, S10088\tS10089 S10090')

    expect(accounts).toEqual(['S10086', 'S10087', 'S10088', 'S10089', 'S10090'])
  })

  it('支持中文逗号与顿号（老师从中文文档里复制名单是常见输入）', () => {
    const { accounts } = parseStudentAccounts('S10086，S10087、S10088')

    expect(accounts).toEqual(['S10086', 'S10087', 'S10088'])
  })

  it('去掉分隔符产生的空项，并保持输入顺序', () => {
    const { accounts } = parseStudentAccounts('  \n\n S10087 ,, S10086 ,\n')

    expect(accounts).toEqual(['S10087', 'S10086'])
  })

  it('去重：同一个账号写两次只提交一次（它是幂等的，两次只会制造噪声）', () => {
    const { accounts } = parseStudentAccounts('S10086 S10086\nS10086')

    expect(accounts).toEqual(['S10086'])
  })

  it('不做大小写归一：账号是凭据，猜错大小写只会把"账号不存在"藏起来', () => {
    const { accounts } = parseStudentAccounts('s10086 S10086')

    expect(accounts).toEqual(['s10086', 'S10086'])
  })

  it('超过单次上限时截断，并报告被丢弃的个数', () => {
    const input = Array.from({ length: 103 }, (_, index) => `S${10000 + index}`).join('\n')

    const { accounts, dropped } = parseStudentAccounts(input)

    expect(accounts).toHaveLength(100)
    expect(accounts[0]).toBe('S10000')
    expect(dropped).toBe(3)
  })

  it('空输入得到空数组（调用方据此拦下注定 400 的请求）', () => {
    expect(parseStudentAccounts('   \n , ，  ')).toEqual({ accounts: [], dropped: 0 })
  })
})

describe('describeAddResult', () => {
  it('全部成功 / 部分成功 / 全部被拒三种说法', () => {
    expect(describeAddResult(3, 0)).toBe('成功加入 3 个学生')
    expect(describeAddResult(2, 1)).toBe('成功加入 2 个学生，1 个账号被拒绝')
    expect(describeAddResult(0, 2)).toBe('没有账号被加入，2 个账号被拒绝')
  })
})
