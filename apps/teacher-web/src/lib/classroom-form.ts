import {
  CLASSROOM_DESCRIPTION_MAX_LENGTH,
  CLASSROOM_NAME_MAX_LENGTH,
  CLASSROOM_NAME_MIN_LENGTH,
} from '@classwatch/shared-types'

/**
 * 课堂名称 / 说明的本地校验（§7；docs/frontend/teacher.md §3）。
 *
 * WHY 单独一层而不是各页写一遍：新建页与详情页的编辑区用的是**同一条**规则
 * （去空白后 1–80 / ≤500），各写一份的结果是两页对同一个输入给出不同的判断。
 *
 * 强调：这层只为了不让老师白填一遍，真正的拒绝永远在后端（§63）。
 * 返回 null 表示本地没发现问题，不代表后端会接受。
 */
export function validateClassroomName(value: string): string | null {
  const length = value.trim().length
  if (length < CLASSROOM_NAME_MIN_LENGTH) return '请输入课堂名称'
  if (length > CLASSROOM_NAME_MAX_LENGTH) return `课堂名称最多 ${CLASSROOM_NAME_MAX_LENGTH} 个字符`
  return null
}

export function validateClassroomDescription(value: string): string | null {
  const length = value.trim().length
  if (length > CLASSROOM_DESCRIPTION_MAX_LENGTH) {
    return `课堂说明最多 ${CLASSROOM_DESCRIPTION_MAX_LENGTH} 个字符`
  }
  return null
}
