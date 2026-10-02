import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import AppButton from '../components/AppButton.vue'
import AppCard from '../components/AppCard.vue'
import AppShell from '../components/AppShell.vue'
import AppTextField from '../components/AppTextField.vue'
import PhasePlaceholder from '../components/PhasePlaceholder.vue'
import ProtectedRouteGate from '../components/ProtectedRouteGate.vue'
import StatusDot from '../components/StatusDot.vue'

describe('AppShell', () => {
  it('渲染品牌、入口副标题与默认插槽内容', () => {
    const wrapper = mount(AppShell, {
      props: { brand: 'ClassWatch', subtitle: '学生端' },
      slots: { default: '<p>content</p>' },
    })

    expect(wrapper.text()).toContain('ClassWatch')
    expect(wrapper.text()).toContain('学生端')
    expect(wrapper.find('main').html()).toContain('content')
  })

  it('没有 userName 时不渲染账号区（登录页不该出现"退出登录"）', () => {
    const wrapper = mount(AppShell, { props: { subtitle: '学生端' } })

    expect(wrapper.find('[data-testid="shell-account"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('退出登录')
  })

  it('传入 userName 时显示当前用户，并在点击时 emit logout（组件自身不认识 store/路由）', async () => {
    const wrapper = mount(AppShell, { props: { userName: '张三' } })

    expect(wrapper.find('[data-testid="shell-user-name"]').text()).toBe('张三')

    await wrapper.find('[data-testid="shell-logout"]').trigger('click')

    expect(wrapper.emitted('logout')).toHaveLength(1)
  })

  it('logoutPending 时禁用退出按钮，避免重复撤销会话', () => {
    const wrapper = mount(AppShell, { props: { userName: '张三', logoutPending: true } })

    expect(wrapper.find('[data-testid="shell-logout"]').attributes('disabled')).toBeDefined()
  })
})

describe('AppTextField', () => {
  it('label 通过 for/id 关联到输入框（点标签能聚焦，读屏能报出字段名）', () => {
    const wrapper = mount(AppTextField, { props: { label: '账号', modelValue: '' } })

    const label = wrapper.find('label')
    const input = wrapper.find('input')
    expect(label.attributes('for')).toBe(input.attributes('id'))
  })

  it('输入时 emit update:modelValue，回车时 emit enter', async () => {
    const wrapper = mount(AppTextField, { props: { label: '账号', modelValue: '' } })

    await wrapper.find('input').setValue('S10086')
    await wrapper.find('input').trigger('keyup.enter')

    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual(['S10086'])
    expect(wrapper.emitted('enter')).toHaveLength(1)
  })

  it('error 优先于 hint，并用 aria-invalid / aria-describedby 表达错误状态', () => {
    const wrapper = mount(AppTextField, {
      props: { label: '账号', modelValue: '', error: '请输入账号', hint: '提示' },
    })

    const input = wrapper.find('input')
    expect(wrapper.text()).toContain('请输入账号')
    expect(wrapper.text()).not.toContain('提示')
    expect(input.attributes('aria-invalid')).toBe('true')
    expect(input.attributes('aria-describedby')).toBeTruthy()
  })

  it('两个实例的 id 不相同（同一页面出现两个字段时 label 不会指错）', () => {
    const first = mount(AppTextField, { props: { label: '账号', modelValue: '' } })
    const second = mount(AppTextField, { props: { label: '密码', modelValue: '' } })

    expect(first.find('input').attributes('id')).not.toBe(second.find('input').attributes('id'))
  })
})

describe('ProtectedRouteGate', () => {
  it('默认提示"正在确认登录状态"，并可通过 hint 说明原因', () => {
    const wrapper = mount(ProtectedRouteGate, { props: { hint: '请检查网络连接。' } })

    expect(wrapper.find('[role="status"]').text()).toContain('正在确认登录状态')
    expect(wrapper.text()).toContain('请检查网络连接。')
  })
})

describe('AppCard', () => {
  it('渲染标题与描述，padded=false 时不加内边距', () => {
    const padded = mount(AppCard, { props: { title: '我的课堂', description: '描述' } })
    expect(padded.text()).toContain('我的课堂')
    expect(padded.text()).toContain('描述')
    expect(padded.find('section').classes()).toContain('p-6')

    const flush = mount(AppCard, { props: { padded: false } })
    expect(flush.find('section').classes()).not.toContain('p-6')
  })
})

describe('AppButton', () => {
  it.each(['primary', 'secondary', 'danger', 'ghost'] as const)('支持 %s 变体', (variant) => {
    const wrapper = mount(AppButton, { props: { variant } })
    expect(wrapper.find('button').exists()).toBe(true)
  })

  it('loading 时禁用按钮并给出 aria-busy，防止重复提交', () => {
    const wrapper = mount(AppButton, { props: { loading: true } })

    expect(wrapper.find('button').attributes('disabled')).toBeDefined()
    expect(wrapper.find('button').attributes('aria-busy')).toBe('true')
  })

  it('disabled 时禁用按钮', () => {
    const wrapper = mount(AppButton, { props: { disabled: true } })
    expect(wrapper.find('button').attributes('disabled')).toBeDefined()
  })
})

describe('StatusDot', () => {
  it('按 status 选择状态色并渲染文案', () => {
    const wrapper = mount(StatusDot, { props: { status: 'danger', label: '屏幕中断' } })

    expect(wrapper.text()).toContain('屏幕中断')
    expect(wrapper.find('span > span').classes()).toContain('bg-status-danger')
  })
})

describe('PhasePlaceholder', () => {
  it('展示标题、职责、Phase 标注、约束列表与路由 path', () => {
    const wrapper = mount(PhasePlaceholder, {
      props: {
        title: '学生登录',
        description: '学生用账号登录，成功后进入我的课堂。',
        phase: 'Phase 1',
        path: '/student/login',
        notes: ['学生账号没有密码，登录只提交 account。', '会话必须走 HttpOnly Cookie。'],
      },
    })

    const text = wrapper.text()
    expect(text).toContain('/student/login')
    expect(text).toContain('学生登录')
    expect(text).toContain('将在 Phase 1 实现')
    expect(wrapper.findAll('li')).toHaveLength(2)
  })

  it('没有约束时不渲染列表', () => {
    const wrapper = mount(PhasePlaceholder, {
      props: { title: 't', description: 'd', phase: 'Phase 2' },
    })

    expect(wrapper.findAll('li')).toHaveLength(0)
  })
})
