import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import AppAlert from '../components/AppAlert.vue'
import AppBadge from '../components/AppBadge.vue'
import AppButton from '../components/AppButton.vue'
import AppCard from '../components/AppCard.vue'
import AppEmptyState from '../components/AppEmptyState.vue'
import AppModal from '../components/AppModal.vue'
import AppPagination from '../components/AppPagination.vue'
import AppSelect from '../components/AppSelect.vue'
import AppShell from '../components/AppShell.vue'
import AppTable from '../components/AppTable.vue'
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

describe('AppBadge', () => {
  it.each([
    ['positive', 'bg-status-open'],
    ['muted', 'bg-status-closed'],
    ['danger', 'bg-status-danger'],
    ['attention', 'bg-status-warning'],
  ] as const)('tone=%s 渲染对应语义色', (tone, dotClass) => {
    const wrapper = mount(AppBadge, { props: { label: '启用', tone } })

    expect(wrapper.text()).toContain('启用')
    expect(wrapper.find('span > span').classes()).toContain(dotClass)
  })
})

describe('AppAlert', () => {
  it('danger 用 role=alert 打断朗读，其余语气用 role=status', () => {
    const danger = mount(AppAlert, { props: { tone: 'danger' }, slots: { default: '失败了' } })
    const info = mount(AppAlert, { props: { tone: 'info' }, slots: { default: '说明' } })

    expect(danger.attributes('role')).toBe('alert')
    expect(info.attributes('role')).toBe('status')
  })

  it('渲染标题、正文与操作区插槽', () => {
    const wrapper = mount(AppAlert, {
      props: { tone: 'success', title: '账号已创建' },
      slots: { default: 'T1001', actions: '<button>知道了</button>' },
    })

    expect(wrapper.text()).toContain('账号已创建')
    expect(wrapper.text()).toContain('T1001')
    expect(wrapper.find('button').exists()).toBe(true)
  })
})

describe('AppEmptyState', () => {
  it('渲染标题、说明与操作插槽', () => {
    const wrapper = mount(AppEmptyState, {
      props: { title: '还没有任何账号', description: '先创建一个老师或学生。' },
      slots: { default: '<button>新建账号</button>' },
    })

    expect(wrapper.attributes('role')).toBe('status')
    expect(wrapper.text()).toContain('还没有任何账号')
    expect(wrapper.text()).toContain('先创建一个老师或学生。')
    expect(wrapper.find('button').exists()).toBe(true)
  })

  it('tone=danger 时用 role=alert 表达失败', () => {
    const wrapper = mount(AppEmptyState, { props: { title: '加载失败', tone: 'danger' } })

    expect(wrapper.attributes('role')).toBe('alert')
  })
})

describe('AppTable', () => {
  it('渲染语义化表格：caption 提供读屏标题，表头/表体来自插槽', () => {
    const wrapper = mount(AppTable, {
      props: { label: '账号列表' },
      slots: {
        head: '<th scope="col">账号</th>',
        default: '<tr><td>T1001</td></tr>',
      },
    })

    expect(wrapper.find('caption').text()).toBe('账号列表')
    expect(wrapper.find('thead th').text()).toBe('账号')
    expect(wrapper.find('tbody td').text()).toBe('T1001')
    // 没有 foot 插槽时不应渲染空的 tfoot（空元素会被读屏当成空行）。
    expect(wrapper.find('tfoot').exists()).toBe(false)
  })
})

describe('AppSelect', () => {
  it('label 关联到 select，选项变化时 emit 字符串值', async () => {
    const wrapper = mount(AppSelect, {
      props: {
        label: '角色',
        modelValue: '',
        options: [
          { value: '', label: '全部角色' },
          { value: 'TEACHER', label: '老师' },
        ],
      },
    })

    const select = wrapper.find('select')
    expect(wrapper.find('label').attributes('for')).toBe(select.attributes('id'))

    await select.setValue('TEACHER')
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual(['TEACHER'])
  })

  it('error 优先于 hint', () => {
    const wrapper = mount(AppSelect, {
      props: { label: '状态', modelValue: '', options: [], error: '请选择', hint: '提示' },
    })

    expect(wrapper.text()).toContain('请选择')
    expect(wrapper.text()).not.toContain('提示')
  })
})

describe('AppPagination', () => {
  it('展示当前区间与总数，首页时"上一页"禁用', async () => {
    const wrapper = mount(AppPagination, {
      props: { page: 1, pageSize: 50, total: 120, unit: '个账号' },
    })

    expect(wrapper.find('[data-testid="pagination-summary"]').text()).toContain('1–50')
    expect(wrapper.find('[data-testid="pagination-summary"]').text()).toContain('共 120')
    expect(wrapper.find('[data-testid="pagination-prev"]').attributes('disabled')).toBeDefined()

    await wrapper.find('[data-testid="pagination-next"]').trigger('click')
    expect(wrapper.emitted('update:page')?.[0]).toEqual([2])
  })

  it('末页时"下一页"禁用，且不会发出越界页码', async () => {
    const wrapper = mount(AppPagination, { props: { page: 3, pageSize: 50, total: 120 } })

    expect(wrapper.find('[data-testid="pagination-summary"]').text()).toContain('101–120')
    expect(wrapper.find('[data-testid="pagination-next"]').attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('update:page')).toBeUndefined()
  })

  it('总数为 0 时显示 0 而不是 1', () => {
    const wrapper = mount(AppPagination, { props: { page: 1, pageSize: 50, total: 0 } })

    expect(wrapper.find('[data-testid="pagination-summary"]').text()).toContain('共 0 条')
  })

  it('页码超出最后一页时按最后一页显示（手改的 URL 不该显示不可能的数字）', () => {
    const wrapper = mount(AppPagination, { props: { page: 99, pageSize: 50, total: 120 } })

    const summary = wrapper.find('[data-testid="pagination-summary"]').text()
    expect(summary).toContain('101–120')
    expect(summary).not.toContain('4901')
    expect(wrapper.text()).toContain('第 3 / 3 页')
  })
})

describe('AppModal', () => {
  it('open=false 时不渲染任何内容（v-if 保证内容真的离开 DOM）', () => {
    const wrapper = mount(AppModal, {
      props: { open: false, title: '一次性密码' },
      slots: { default: 'super-secret' },
    })

    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('super-secret')
  })

  it('open 时渲染对话框：aria-modal、标题关联、焦点进入面板', async () => {
    // attachTo 是必须的：焦点只有在元素真的位于 document 里时才会生效，
    // 游离的测试节点上 document.activeElement 永远是 body。
    const wrapper = mount(AppModal, {
      props: { open: true, title: '确认停用', description: '停用会立即断开会话' },
      attachTo: document.body,
    })
    await flushPromises()

    const dialog = wrapper.find('[role="dialog"]')
    expect(dialog.attributes('aria-modal')).toBe('true')
    expect(dialog.attributes('aria-labelledby')).toBe(wrapper.find('h2').attributes('id'))
    expect(dialog.attributes('aria-describedby')).toBe(wrapper.find('p').attributes('id'))
    expect(document.activeElement).toBe(dialog.element)

    wrapper.unmount()
  })

  it('ESC 与点击遮罩都会 emit close', async () => {
    const wrapper = mount(AppModal, { props: { open: true, title: '确认' } })
    await flushPromises()

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await wrapper.find('[data-testid="modal-overlay"]').trigger('click')

    expect(wrapper.emitted('close')).toHaveLength(2)
    wrapper.unmount()
  })

  it('dismissible=false 时 ESC 与遮罩都不能关闭（危险确认必须点按钮）', async () => {
    const wrapper = mount(AppModal, {
      props: { open: true, title: '确认停用', dismissible: false },
    })
    await flushPromises()

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await wrapper.find('[data-testid="modal-overlay"]').trigger('click')

    expect(wrapper.emitted('close')).toBeUndefined()
    wrapper.unmount()
  })
})
