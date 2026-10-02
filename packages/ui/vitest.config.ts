import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [vue()],
  test: {
    // 组件测试需要 DOM：happy-dom 比 jsdom 快，足够覆盖挂载/属性/插槽断言。
    environment: 'happy-dom',
    include: ['src/**/*.spec.ts'],
  },
})
