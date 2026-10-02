import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    // HTTP 语义仍用注入的 fake fetch 覆盖，绝不打真实网络。
    // 但 CSRF provider 必须读 document.cookie（§63 双提交模式），node 环境没有
    // document，用 happy-dom 让测试跑在真实浏览器语义下，而不是给 CSRF 代码加
    // "测试时走另一条分支"的可测性后门。
    environment: 'happy-dom',
    include: ['src/**/*.spec.ts'],
  },
})
