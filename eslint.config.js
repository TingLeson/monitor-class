import { defineConfigWithVueTs, vueTsConfigs } from '@vue/eslint-config-typescript'
import prettierConfig from 'eslint-config-prettier/flat'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'

/**
 * ClassWatch 前端 ESLint（ESLint 10 / flat config）。
 *
 * 组合方式：eslint-plugin-vue flat/recommended（Vue 模板规则）+ typescript-eslint
 * recommended（TS 规则，非类型感知）+ eslint-config-prettier（关掉所有与 Prettier
 * 冲突的格式规则，格式问题只由 `pnpm format:check` 一处负责）。
 *
 * 这里刻意不使用 recommendedTypeChecked：类型感知规则要求每个文件都落在某个
 * tsconfig 的 include 内，Monorepo 里三套 app + 三个包的 project 列表极难维护，
 * 而且 `pnpm -r typecheck` 已经用 vue-tsc 做了真正的类型检查，不必让 lint 重复
 * 承担这件事（§79：每个 Phase 必须 typecheck 通过）。
 */
const vueRecommended = pluginVue.configs['flat/recommended']

/**
 * eslint-plugin-vue 的 recommended 预设把一部分规则定为 "warn"，而 CI 使用
 * `eslint . --max-warnings=0`，warn 会直接变成失败。与其让这些规则静默失效，
 * 不如在这里显式提升为 error：规则集完全一致，只是不再有"被忽略的警告"。
 */
const vueWarningsAsErrors = Object.fromEntries(
  vueRecommended
    .flatMap((config) => Object.entries(config.rules ?? {}))
    .map(([rule, severity]) => [rule, Array.isArray(severity) ? severity : [severity]])
    .filter(([, severity]) => severity[0] === 'warn' || severity[0] === 1)
    .map(([rule, severity]) => [rule, ['error', ...severity.slice(1)]]),
)

export default defineConfigWithVueTs(
  {
    name: 'classwatch/ignores',
    ignores: [
      '**/dist/**',
      '**/coverage/**',
      '**/node_modules/**',
      '**/*.tsbuildinfo',
      // 后端与任务书不属于前端 lint 范围。
      'services/**',
      '开发任务书.md',
    ],
  },
  {
    name: 'classwatch/globals',
    files: ['**/*.{js,mjs,cjs,ts,mts,vue}'],
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      // 三个 SPA 都跑在浏览器里；浏览器全局（window/document/navigator）在此声明。
      globals: { ...globals.browser },
    },
  },
  {
    // 构建/测试配置文件跑在 Node 里（process、__dirname 等）。
    name: 'classwatch/node-config-files',
    files: ['**/*.config.{js,mjs,ts}', '**/vite.config.ts', '**/vitest.config.ts'],
    languageOptions: {
      globals: { ...globals.node },
    },
  },
  pluginVue.configs['flat/recommended'],
  vueTsConfigs.recommended,
  {
    name: 'classwatch/warnings-as-errors',
    rules: vueWarningsAsErrors,
  },
  {
    name: 'classwatch/rules',
    rules: {
      // Vue 3 + <script setup> 下多词组件名要求过严（视图文件名即组件名，已在路由中固定）。
      'vue/multi-word-component-names': ['error', { ignores: ['App'] }],
      // 未使用参数允许用下划线前缀显式表达"我故意不用"。
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_', caughtErrorsIgnorePattern: '^_' },
      ],
      // 前后端契约依赖显式类型（shared-types 是唯一类型来源），禁止隐式 any 泄漏。
      '@typescript-eslint/no-explicit-any': 'error',
      'no-console': ['error', { allow: ['warn', 'error'] }],
    },
  },
  {
    // 测试文件里允许使用非空断言（fixture 数据与 spy 断言更易读）。
    name: 'classwatch/tests',
    files: ['**/*.spec.ts', '**/__tests__/**/*.ts'],
    rules: {
      '@typescript-eslint/no-non-null-assertion': 'off',
    },
  },
  prettierConfig,
)
