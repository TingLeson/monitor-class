import { fileURLToPath } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig, loadEnv } from 'vite'

/** 仓库根目录：三个 app 共用同一份 .env，避免端口/地址在多处重复配置后漂移。 */
const repoRoot = fileURLToPath(new URL('../..', import.meta.url))

/**
 * 管理端 dev server。
 *
 * WHY 端口写死 + strictPort：后端 CORS allowlist 是按来源写死的
 * （.env.example: http://localhost:5173,5174,5175），端口一旦漂移，
 * 表现是"登录/接口突然失败"这类极难定位的 CORS / Cookie 问题。
 * 宁可启动直接失败，也不要静默换端口。
 */
export default defineConfig(({ mode }) => {
  // 代理目标从仓库根的 .env 读取，保证与后端实际发布端口一致。
  const env = loadEnv(mode, repoRoot, '')
  const apiTarget = env.VITE_API_PROXY_TARGET ?? 'http://localhost:8090'

  return {
    plugins: [vue(), tailwindcss()],
    server: {
      port: 5175,
      strictPort: true,
      proxy: {
        // 开发环境走同源代理：前端 baseUrl 留空，请求 /api/v1/... 由 Vite 转发到后端。
        '/api': { target: apiTarget, changeOrigin: true },
        '/healthz': { target: apiTarget, changeOrigin: true },
        '/readyz': { target: apiTarget, changeOrigin: true },
      },
    },
  }
})
