/**
 * @classwatch/ui —— 三个 Web 入口共享的展示层。
 *
 * 边界（必须保持）：组件只描述"长什么样"，不含业务逻辑、不发请求、不读路由、
 * 不访问 Pinia。业务页面在各 app 的 src/views 里组装这些组件。
 *
 * 样式入口是 `@classwatch/ui/styles.css`（Tailwind v4 主题 + @source），
 * 由 app 的 src/style.css 引入，组件自身不 import CSS，避免重复注入主题。
 */

export { default as AppAlert } from './components/AppAlert.vue'
export { default as AppBadge } from './components/AppBadge.vue'
export { default as AppButton } from './components/AppButton.vue'
export { default as AppCard } from './components/AppCard.vue'
export { default as AppEmptyState } from './components/AppEmptyState.vue'
export { default as AppModal } from './components/AppModal.vue'
export { default as AppPagination } from './components/AppPagination.vue'
export { default as AppSelect } from './components/AppSelect.vue'
export { default as AppShell } from './components/AppShell.vue'
export { default as AppTable } from './components/AppTable.vue'
export { default as AppTextArea } from './components/AppTextArea.vue'
export { default as AppTextField } from './components/AppTextField.vue'
export { default as PhasePlaceholder } from './components/PhasePlaceholder.vue'
export { default as ProtectedRouteGate } from './components/ProtectedRouteGate.vue'
export { default as StatusDot } from './components/StatusDot.vue'

export type { AlertTone } from './components/AppAlert.vue'
export type { BadgeTone } from './components/AppBadge.vue'
export type { ButtonSize, ButtonVariant } from './components/AppButton.vue'
export type { SelectOption } from './components/AppSelect.vue'
export type { StatusTone } from './components/StatusDot.vue'
