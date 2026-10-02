import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    // 客户端不依赖 DOM：只有 HTTP 语义，测试用注入的 fake fetch，绝不打真实网络。
    environment: 'node',
    include: ['src/**/*.spec.ts'],
  },
})
