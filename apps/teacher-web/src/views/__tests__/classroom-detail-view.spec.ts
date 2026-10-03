import { ApiError } from '@classwatch/api-client'
import { DOMWrapper, flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  makeClassroom,
  makeClassroomRun,
  makeClassroomStudent,
  makeOpenClassroom,
} from '../../__tests__/fixtures'
import { installFakeRealtimeSocket, makeRoomClosedEvent } from '../../__tests__/realtime-fixtures'
import { routes } from '../../router'
import { useClassroomsStore } from '../../stores/classrooms'
import { useRealtimeStore } from '../../stores/realtime'
import ClassroomDetailView from '../ClassroomDetailView.vue'

/**
 * 课堂详情页测试（§7 / §11 / §48 / §49；docs/frontend/teacher.md §3–§5）。
 *
 * 重点覆盖三类"错了会骗人"的行为：
 * 1. 开关课堂的真实状态（成功用返回的 DTO、冲突要刷新并说明）；
 * 2. 批量添加的**部分成功**必须逐条展示，不能吞掉 rejected；
 * 3. 切换 :id 时上一个课堂的名单不能留在页面上（§14：名单不跨课堂）。
 */
const {
  getClassroomMock,
  listStudentsMock,
  addStudentsMock,
  removeStudentMock,
  openClassroomMock,
  updateClassroomMock,
} = vi.hoisted(() => ({
  getClassroomMock: vi.fn(),
  listStudentsMock: vi.fn(),
  addStudentsMock: vi.fn(),
  removeStudentMock: vi.fn(),
  openClassroomMock: vi.fn(),
  updateClassroomMock: vi.fn(),
}))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: vi.fn().mockResolvedValue([]),
  getClassroom: getClassroomMock,
  createClassroom: vi.fn(),
  updateClassroom: updateClassroomMock,
  listClassroomStudents: listStudentsMock,
  addClassroomStudents: addStudentsMock,
  removeClassroomStudent: removeStudentMock,
  openClassroom: openClassroomMock,
  closeClassroom: vi.fn(),
}))

const CLOSED = makeClassroom({ id: 'room-1', name: 'C++ 算法训练', studentCount: 1 })
const STUDENT = makeClassroomStudent()

async function mountView(
  path = '/teacher/classrooms/room-1',
): Promise<{ wrapper: VueWrapper; router: Router }> {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  const wrapper = mount(ClassroomDetailView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

async function submitForm(wrapper: VueWrapper, testid: string): Promise<void> {
  // 详情页有两个表单（编辑信息 / 批量添加），必须提交 testid 所在的那一个，
  // 否则测试会"提交了另一个表单却以为验证了本表单"。
  const form = wrapper.find(`[data-testid="${testid}"]`).element.closest('form')
  if (!form) throw new Error(`${testid} 不在表单里`)
  await new DOMWrapper(form).trigger('submit')
  await flushPromises()
}

describe('课堂详情页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    getClassroomMock.mockReset().mockResolvedValue(CLOSED)
    listStudentsMock.mockReset().mockResolvedValue({ students: [STUDENT] })
    addStudentsMock.mockReset()
    removeStudentMock.mockReset().mockResolvedValue(undefined)
    openClassroomMock.mockReset()
    updateClassroomMock.mockReset().mockResolvedValue(CLOSED)
  })

  it('渲染头部状态、学生数与名单（账号、显示名、状态徽章、加入时间）', async () => {
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="detail-name"]').text()).toBe('C++ 算法训练')
    expect(wrapper.find('[data-testid="detail-status"]').text()).toContain('未开启')
    expect(wrapper.find('[data-testid="detail-meta"]').text()).toContain('学生 1 人')

    const row = wrapper.find('[data-testid="student-row-user-student-1"]')
    expect(row.text()).toContain('S10086')
    expect(row.text()).toContain('张三')
    expect(row.text()).toContain('正常')
    expect(row.text()).toContain('2026') // 加入时间按本地格式展示
  })

  it('名单为空时显示空状态，并提示用下面的输入框添加', async () => {
    listStudentsMock.mockResolvedValue({ students: [] })
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="students-empty"]').text()).toContain('名单里还没有学生')
    expect(wrapper.find('[data-testid="student-row-user-student-1"]').exists()).toBe(false)
  })

  it('已停用的学生仍然显示，但明确说明他进不来（授权关系与账号状态是两件事）', async () => {
    listStudentsMock.mockResolvedValue({
      students: [makeClassroomStudent({ status: 'DISABLED' })],
    })
    const { wrapper } = await mountView()

    const row = wrapper.find('[data-testid="student-row-user-student-1"]')
    expect(row.text()).toContain('已停用')
    expect(row.text()).toContain('无法进入课堂')
  })

  it('移除学生：确认之前不发请求，确认后调用接口并重新拉名单', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="remove-student-user-student-1"]').trigger('click')
    expect(removeStudentMock).not.toHaveBeenCalled()

    listStudentsMock.mockResolvedValue({ students: [] })
    await wrapper.find('[data-testid="confirm-remove"]').trigger('click')
    await flushPromises()

    expect(removeStudentMock).toHaveBeenCalledWith('room-1', 'user-student-1')
    // 204 没有响应体：名单以后端返回为准，被移除的人必须从页面上消失。
    expect(listStudentsMock).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="student-row-user-student-1"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="roster-notice"]').text()).toContain('移出名单')
  })

  it('STUDENT_NOT_ASSIGNED：说明该学生本来就不在名单里，并刷新名单', async () => {
    removeStudentMock.mockRejectedValue(new ApiError({ code: 'STUDENT_NOT_ASSIGNED', status: 404 }))
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="remove-student-user-student-1"]').trigger('click')
    await wrapper.find('[data-testid="confirm-remove"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="roster-notice"]').text()).toContain('不在名单里')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('批量添加：展示"成功加入 N 个"以及逐条 rejected 原因，并刷新名单', async () => {
    addStudentsMock.mockResolvedValue({
      students: [STUDENT],
      rejected: [
        { account: 'S99999', code: 'STUDENT_NOT_FOUND' },
        { account: 'T1001', code: 'NOT_A_STUDENT' },
      ],
    })
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="add-accounts"] textarea').setValue('S10086\nS99999 T1001')
    await submitForm(wrapper, 'add-accounts')

    expect(addStudentsMock).toHaveBeenCalledWith('room-1', ['S10086', 'S99999', 'T1001'])
    const result = wrapper.find('[data-testid="add-result"]')
    expect(result.text()).toContain('成功加入 1 个学生')
    expect(result.text()).toContain('2 个账号被拒绝')
    // 逐条：账号 + 中文原因（老师据此改哪一个账号）。
    expect(wrapper.find('[data-testid="rejected-S99999"]').text()).toContain('账号不存在')
    expect(wrapper.find('[data-testid="rejected-T1001"]').text()).toContain('不是学生账号')
    // 被拒的账号回填到输入框，方便直接改一个再提交。
    expect(
      (wrapper.find('[data-testid="add-accounts"] textarea').element as HTMLTextAreaElement).value,
    ).toBe('S99999\nT1001')
  })

  it('批量添加全成功时清空输入框，不留下刚提交的账号', async () => {
    addStudentsMock.mockResolvedValue({ students: [STUDENT], rejected: [] })
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="add-accounts"] textarea').setValue('S10086, S10087')
    await submitForm(wrapper, 'add-accounts')

    expect(wrapper.find('[data-testid="add-result"]').text()).toContain('成功加入 2 个学生')
    expect(
      (wrapper.find('[data-testid="add-accounts"] textarea').element as HTMLTextAreaElement).value,
    ).toBe('')
  })

  it('批量添加一个账号都没填时不发请求', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="add-accounts"] textarea').setValue('  \n , ')
    await submitForm(wrapper, 'add-accounts')

    expect(addStudentsMock).not.toHaveBeenCalled()
  })

  it('整批被拒（NOT_A_STUDENT）时把原因挂到账号输入框下方，而不是页面顶部', async () => {
    addStudentsMock.mockRejectedValue(new ApiError({ code: 'NOT_A_STUDENT', status: 400 }))
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="add-accounts"] textarea').setValue('T1001')
    await submitForm(wrapper, 'add-accounts')

    expect(wrapper.find('[data-testid="add-accounts"]').text()).toContain('不是学生账号')
    expect(wrapper.find('[data-testid="add-error"]').exists()).toBe(false)
  })

  it('批量添加失败（网络错误）时给出提示且不显示结果块', async () => {
    addStudentsMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR' }))
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="add-accounts"] textarea').setValue('S10086')
    await submitForm(wrapper, 'add-accounts')

    expect(wrapper.find('[data-testid="add-error"]').text()).toContain('网络连接失败')
    expect(wrapper.find('[data-testid="add-result"]').exists()).toBe(false)
  })

  it('编辑信息：保存后页面数据更新，按钮在没有改动时是禁用的', async () => {
    const { wrapper } = await mountView()
    expect(wrapper.find('[data-testid="save-classroom"]').attributes('disabled')).toBeDefined()

    updateClassroomMock.mockResolvedValue(makeClassroom({ id: 'room-1', name: '改过的名字' }))
    await wrapper.find('[data-testid="edit-name"] input').setValue('改过的名字')
    await submitForm(wrapper, 'edit-name')

    expect(updateClassroomMock).toHaveBeenCalledWith('room-1', {
      name: '改过的名字',
      description: '第三章 动态规划',
    })
    expect(wrapper.find('[data-testid="detail-name"]').text()).toBe('改过的名字')
    expect(wrapper.find('[data-testid="save-notice"]').text()).toContain('已保存')
  })

  it('编辑信息：名称为空时本地拦下，不发请求', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="edit-name"] input').setValue('   ')
    await submitForm(wrapper, 'edit-name')

    expect(updateClassroomMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="edit-name"]').text()).toContain('请输入课堂名称')
  })

  it('详情 404：提示课堂不存在、清空数据，并给回列表入口', async () => {
    getClassroomMock.mockRejectedValue(new ApiError({ code: 'CLASSROOM_NOT_FOUND', status: 404 }))
    const { wrapper } = await mountView()

    const error = wrapper.find('[data-testid="detail-error"]')
    expect(error.text()).toContain('课堂不存在')
    expect(error.find('[data-testid="detail-back"]').attributes('href')).toBe('/teacher/classrooms')
    // 失败时不能留着上一个课堂的数据（这里连带名单也要为空）。
    expect(wrapper.find('[data-testid="student-row-user-student-1"]').exists()).toBe(false)
  })

  it('不是自己的课堂（403 CLASSROOM_NOT_OWNER）：说明原因并给回列表入口', async () => {
    getClassroomMock.mockRejectedValue(new ApiError({ code: 'CLASSROOM_NOT_OWNER', status: 403 }))
    const { wrapper } = await mountView()

    expect(wrapper.find('[data-testid="detail-error"]').text()).toContain('只有课堂的创建老师')
    expect(wrapper.find('[data-testid="detail-back"]').exists()).toBe(true)
  })

  it(':id 变化时重新拉取，并且不把上一个课堂的名单留在页面上（§14）', async () => {
    const { wrapper, router } = await mountView()
    expect(wrapper.find('[data-testid="student-row-user-student-1"]').exists()).toBe(true)

    let resolveStudents: (value: unknown) => void = () => undefined
    listStudentsMock.mockReturnValue(
      new Promise((resolve) => {
        resolveStudents = resolve
      }),
    )
    getClassroomMock.mockResolvedValue(makeClassroom({ id: 'room-2', name: '另一个课堂' }))
    await router.push('/teacher/classrooms/room-2')
    await flushPromises()

    expect(getClassroomMock).toHaveBeenLastCalledWith('room-2')
    // 新课堂的名单还没回来时，页面上不能出现上一个课堂的学生。
    expect(wrapper.find('[data-testid="student-row-user-student-1"]').exists()).toBe(false)

    resolveStudents({
      students: [makeClassroomStudent({ id: 'user-student-9', account: 'S10099' })],
    })
    await flushPromises()

    expect(wrapper.find('[data-testid="detail-name"]').text()).toBe('另一个课堂')
    expect(wrapper.find('[data-testid="student-row-user-student-9"]').text()).toContain('S10099')
  })

  it('开启课堂：确认后调用接口，并用返回的 DTO 更新头部状态与本次开始时间', async () => {
    const { wrapper } = await mountView()
    openClassroomMock.mockResolvedValue({
      classroom: makeOpenClassroom({
        id: 'room-1',
        name: 'C++ 算法训练',
        currentRun: { id: 'run-7', openedAt: '2026-02-03T06:05:00Z' },
      }),
      run: makeClassroomRun({ id: 'run-7', classroomId: 'room-1' }),
    })

    await wrapper.find('[data-testid="detail-toggle"]').trigger('click')
    expect(openClassroomMock).not.toHaveBeenCalled()
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await flushPromises()

    expect(openClassroomMock).toHaveBeenCalledWith('room-1')
    expect(wrapper.find('[data-testid="detail-status"]').text()).toContain('已开启')
    expect(wrapper.find('[data-testid="detail-meta"]').text()).toContain('本次开始于')
    expect(wrapper.find('[data-testid="detail-toggle-notice"]').text()).toContain('已开启')
  })

  it('CLASSROOM_ALREADY_OPEN：说明真实状态并重新取详情（不是"操作失败"）', async () => {
    const { wrapper } = await mountView()
    openClassroomMock.mockRejectedValue(
      new ApiError({ code: 'CLASSROOM_ALREADY_OPEN', status: 409 }),
    )
    getClassroomMock.mockResolvedValue(makeOpenClassroom({ id: 'room-1' }))

    await wrapper.find('[data-testid="detail-toggle"]').trigger('click')
    await wrapper.find('[data-testid="confirm-toggle"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="detail-toggle-notice"]').text()).toContain(
      '课堂已经是开启状态，列表已刷新。',
    )
    expect(getClassroomMock).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="detail-status"]').text()).toContain('已开启')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('离开页面时清空 store 里的详情与名单（数据不该比页面活得更久）', async () => {
    const { wrapper } = await mountView()
    const store = useClassroomsStore()
    expect(store.students).toHaveLength(1)

    wrapper.unmount()

    expect(store.current).toBeNull()
    expect(store.students).toEqual([])
  })

  it('§49：另一个标签页关掉课堂时，本页跟着变成"未开启"并给出说明', async () => {
    getClassroomMock.mockResolvedValue(makeOpenClassroom({ id: 'room-1', name: 'C++ 算法训练' }))
    const { wrapper } = await mountView()
    expect(wrapper.find('[data-testid="detail-status"]').text()).toContain('已开启')

    const socket = installFakeRealtimeSocket()
    const realtime = useRealtimeStore()
    realtime.start()
    socket.open()
    // 事件**同步**写进 store（DOM 要等一次渲染；HTTP 快照这时还没回来，
    // 所以这个状态只可能来自实时事件，§47）。
    socket.emit(makeRoomClosedEvent({ classroomId: 'room-1' }))
    expect(useClassroomsStore().current?.status).toBe('CLOSED')

    // 快照随后收敛：真实后端在课堂关闭后返回的也是 CLOSED。
    getClassroomMock.mockResolvedValue(makeClassroom({ id: 'room-1', status: 'CLOSED' }))
    await flushPromises()

    expect(wrapper.find('[data-testid="detail-status"]').text()).toContain('未开启')
    const notice = wrapper.find('[data-testid="detail-realtime-notice"]')
    expect(notice.exists()).toBe(true)
    expect(notice.text()).toContain('其他页面')
    // 关闭后按钮回到"开启课堂"（老师可以再开一节，§8：那是新的 Run）。
    expect(wrapper.find('[data-testid="detail-toggle"]').text()).toContain('开启课堂')
  })
})
