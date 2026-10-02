import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [vue()],
  test: {
    // 组件测试需要 DOM（挂载、路由跳转）；happy-dom 足够快且不依赖浏览器。
    environment: 'happy-dom',
    // 只跑本 app 的源码测试，避免误抓 e2e 或构建产物。
    include: ['src/**/*.spec.ts'],
  },
})
