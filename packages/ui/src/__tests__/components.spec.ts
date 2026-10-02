import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import AppButton from '../components/AppButton.vue'
import AppCard from '../components/AppCard.vue'
import AppShell from '../components/AppShell.vue'
import PhasePlaceholder from '../components/PhasePlaceholder.vue'
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
