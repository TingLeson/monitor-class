import { describe, expect, it } from 'vitest'
import { validateClassroomDescription, validateClassroomName } from '../classroom-form'

/**
 * 课堂表单的本地校验（§7；docs/frontend/teacher.md §3）。
 *
 * 规则只有两条，但每条都容易写错一两个字符（长度算原始值还是去空白后的值、
 * 边界取不取等号），而错了的表现是"老师填对却被前端拦住"或反之。
 */
describe('validateClassroomName', () => {
  it('去空白后为空 → 必填错误（只打空格不能当名称）', () => {
    expect(validateClassroomName('')).toBe('请输入课堂名称')
    expect(validateClassroomName('   \n ')).toBe('请输入课堂名称')
  })

  it('边界：1 与 80 字符合法，81 字符报错', () => {
    expect(validateClassroomName('课')).toBeNull()
    expect(validateClassroomName('课'.repeat(80))).toBeNull()
    expect(validateClassroomName('课'.repeat(81))).toBe('课堂名称最多 80 个字符')
  })

  it('首尾空白不计入长度（后端也是按去空白后的结果判断）', () => {
    expect(validateClassroomName(`  ${'课'.repeat(80)}  `)).toBeNull()
  })
})

describe('validateClassroomDescription', () => {
  it('空说明合法（契约里空串等于未填写，§7）', () => {
    expect(validateClassroomDescription('')).toBeNull()
    expect(validateClassroomDescription('   ')).toBeNull()
  })

  it('边界：500 字符合法，501 字符报错', () => {
    expect(validateClassroomDescription('说'.repeat(500))).toBeNull()
    expect(validateClassroomDescription('说'.repeat(501))).toBe('课堂说明最多 500 个字符')
  })
})
