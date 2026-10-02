import { ApiError } from '@classwatch/api-client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { makeClassroom } from '../../__tests__/fixtures'
import { routes } from '../../router'
import ClassroomNewView from '../ClassroomNewView.vue'

/**
 * 新建课堂页测试（§7；docs/frontend/teacher.md §3）。
 *
 * 两条必须钉住的行为：本地校验（名称必填 / 长度）与服务端错误的落点
 * （INVALID_REQUEST 走页面级提示，不硬塞进某个输入框）。
 */
const { createClassroomMock } = vi.hoisted(() => ({ createClassroomMock: vi.fn() }))

vi.mock('../../lib/teacher-classrooms-api.ts', () => ({
  listClassrooms: vi.fn().mockResolvedValue([]),
  getClassroom: vi.fn(),
  createClassroom: createClassroomMock,
  updateClassroom: vi.fn(),
  listClassroomStudents: vi.fn(),
  addClassroomStudents: vi.fn(),
  removeClassroomStudent: vi.fn(),
  openClassroom: vi.fn(),
  closeClassroom: vi.fn(),
}))

/**
 * 提交表单。
 *
 * WHY 触发 form 的 submit 而不是点按钮：happy-dom 不会因为派发 click 就执行浏览器的
 * 隐式表单提交，于是"点了按钮但什么都没发生"会伪装成通过；触发 submit 才是真实的
 * 提交路径（admin-web 的表单测试用同一手法）。
 */
async function submitForm(wrapper: ReturnType<typeof mount>): Promise<void> {
  await wrapper.find('form').trigger('submit')
  await flushPromises()
}

async function mountView(): Promise<{ wrapper: ReturnType<typeof mount>; router: Router }> {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/teacher/classrooms/new')
  await router.isReady()
  const wrapper = mount(ClassroomNewView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('新建课堂页', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    createClassroomMock.mockReset().mockResolvedValue(makeClassroom({ id: 'room-9' }))
  })

  it('名称为空时给出字段级错误，且不发请求', async () => {
    const { wrapper } = await mountView()

    await submitForm(wrapper)
    await flushPromises()

    expect(wrapper.find('[data-testid="classroom-name"]').text()).toContain('请输入课堂名称')
    expect(createClassroomMock).not.toHaveBeenCalled()
  })

  it('名称只有空白字符同样算未填写（后端会按去空白后的结果拒绝）', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="classroom-name"] input').setValue('    ')
    await submitForm(wrapper)
    await flushPromises()

    expect(createClassroomMock).not.toHaveBeenCalled()
  })

  it('名称超过 80 字符时给出字段级错误，实时字数提示跟着输入变化', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="classroom-name"] input').setValue('课'.repeat(81))
    expect(wrapper.find('[data-testid="classroom-name"]').text()).toContain('81 / 80')

    await submitForm(wrapper)
    await flushPromises()

    expect(wrapper.find('[data-testid="classroom-name"]').text()).toContain('最多 80 个字符')
    expect(createClassroomMock).not.toHaveBeenCalled()
  })

  it('说明超过 500 字符时同样在本地拦下', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="classroom-name"] input').setValue('C++ 算法训练')
    await wrapper.find('[data-testid="classroom-description"] textarea').setValue('说'.repeat(501))
    await submitForm(wrapper)
    await flushPromises()

    expect(wrapper.find('[data-testid="classroom-description"]').text()).toContain(
      '最多 500 个字符',
    )
    expect(createClassroomMock).not.toHaveBeenCalled()
  })

  it('提交成功：只发名称与说明（不带 owner），并跳到详情页', async () => {
    const { wrapper, router } = await mountView()

    await wrapper.find('[data-testid="classroom-name"] input').setValue('  C++ 算法训练  ')
    await wrapper.find('[data-testid="classroom-description"] textarea').setValue('第三章 动态规划')
    await submitForm(wrapper)
    await flushPromises()

    expect(createClassroomMock).toHaveBeenCalledWith({
      name: 'C++ 算法训练',
      description: '第三章 动态规划',
    })
    expect(router.currentRoute.value.name).toBe('teacher-classroom-detail')
    expect(router.currentRoute.value.params.id).toBe('room-9')
  })

  it('说明留空时提交 null（契约里空串等于未填写，§7）', async () => {
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="classroom-name"] input').setValue('C++ 算法训练')
    await submitForm(wrapper)
    await flushPromises()

    expect(createClassroomMock).toHaveBeenCalledWith({
      name: 'C++ 算法训练',
      description: null,
    })
  })

  it('服务端 INVALID_REQUEST：顶部提示采用后端的中文说明，且不跳转', async () => {
    createClassroomMock.mockRejectedValue(
      new ApiError({
        code: 'INVALID_REQUEST',
        message: '课堂名称不能超过 80 个字符',
        status: 400,
      }),
    )
    const { wrapper, router } = await mountView()

    await wrapper.find('[data-testid="classroom-name"] input').setValue('C++ 算法训练')
    await submitForm(wrapper)
    await flushPromises()

    expect(wrapper.find('[data-testid="create-error"]').text()).toContain(
      '课堂名称不能超过 80 个字符',
    )
    // 失败后必须留在原页，让老师改完再提交。
    expect(router.currentRoute.value.name).toBe('teacher-classroom-new')
  })

  it('网络失败时显示通用文案，而不是把原始异常或 HTML 显示出来（§58）', async () => {
    createClassroomMock.mockRejectedValue(new ApiError({ code: 'NETWORK_ERROR' }))
    const { wrapper } = await mountView()

    await wrapper.find('[data-testid="classroom-name"] input').setValue('C++ 算法训练')
    await submitForm(wrapper)
    await flushPromises()

    expect(wrapper.find('[data-testid="create-error"]').text()).toContain('网络连接失败')
  })
})
