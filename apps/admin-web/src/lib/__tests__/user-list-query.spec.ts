import { describe, expect, it } from 'vitest'
import { buildUserListQuery, parseUserListQuery } from '../user-list-query'

/**
 * 筛选条件 ↔ URL query 的纯函数测试（docs/frontend/admin.md §2.1）。
 *
 * 这些用例钉住两件事：**URL 是不可信输入**（非法值不能进 store、更不能发给后端），
 * 以及**默认值不写回地址栏**（筛选没变时 URL 就不该变，否则历史记录会被塞满）。
 */
describe('解析账号列表的 URL query', () => {
  it('空 query 得到默认条件', () => {
    expect(parseUserListQuery({})).toEqual({ q: '', role: null, status: null, page: 1 })
  })

  it('解析合法筛选与页码，并去掉搜索词两端空格', () => {
    expect(
      parseUserListQuery({ q: '  zha  ', role: 'STUDENT', status: 'ACTIVE', page: '3' }),
    ).toEqual({ q: 'zha', role: 'STUDENT', status: 'ACTIVE', page: 3 })
  })

  it('非法值一律当作未筛选，而不是抛错或原样透传', () => {
    expect(parseUserListQuery({ role: 'WIZARD', status: 'SLEEPING' })).toEqual({
      q: '',
      role: null,
      status: null,
      page: 1,
    })
  })

  it.each(['abc', '0', '-3', '1.5', ''])('页码 %s 落到第 1 页', (raw) => {
    expect(parseUserListQuery({ page: raw }).page).toBe(1)
  })

  it('同一个 key 出现多次时取第一个（手工拼接的链接不该让页面崩掉）', () => {
    expect(parseUserListQuery({ q: ['a', 'b'], role: 'TEACHER' }).q).toBe('a')
  })
})

describe('生成账号列表的 URL query', () => {
  it('默认条件写成空 query（干净的 /admin/users）', () => {
    expect(buildUserListQuery({ q: '', role: null, status: null, page: 1 })).toEqual({})
  })

  it('只写非默认值', () => {
    expect(buildUserListQuery({ q: 'zha', role: 'TEACHER', status: null, page: 4 })).toEqual({
      q: 'zha',
      role: 'TEACHER',
      page: '4',
    })
  })

  it('解析与生成互为逆运算（刷新后筛选条件保持不变）', () => {
    const state = { q: 'li', role: 'TEACHER' as const, status: 'ACTIVE' as const, page: 2 }

    expect(parseUserListQuery(buildUserListQuery(state))).toEqual(state)
  })
})
