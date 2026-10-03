import { fileURLToPath } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig, loadEnv } from 'vite'

/** 仓库根目录：三个 app 共用同一份 .env，避免端口/地址在多处重复配置后漂移。 */
const repoRoot = fileURLToPath(new URL('../..', import.meta.url))

/**
 * 学生端 dev server。
 *
 * WHY 端口写死 + strictPort：后端 CORS allowlist 是按来源写死的
 * （.env.example: http://localhost:5173,5174,5175），端口一旦漂移，
 * 表现是"登录/接口突然失败"这类极难定位的 CORS / Cookie 问题。
 * 宁可启动直接失败，也不要静默换端口。
 */
export default defineConfig(({ mode }) => {
  // 代理目标从仓库根的 .env 读取：后端发布端口（API_PORT）与这里必须一致，
  // 写死常量的结果是"后端在 8090、代理打 8080"这种静默失配。
  const env = loadEnv(mode, repoRoot, '')
  const apiTarget = env.VITE_API_PROXY_TARGET ?? 'http://localhost:8090'

  return {
    plugins: [vue(), tailwindcss()],
    server: {
      port: 5173,
      strictPort: true,
      proxy: {
        // 开发环境走同源代理：前端 baseUrl 留空，请求 /api/v1/... 由 Vite 转发到后端。
        // 这样本地不需要配 CORS 白名单与 SameSite=None，Cookie 会话行为与生产一致。
        '/api': { target: apiTarget, changeOrigin: true },
        /**
         * Business Realtime（§47）走同源代理：页面连 `ws(s)://<当前 host>/ws/...`，
         * 由 Vite 转发给后端。`ws: true` 是**必须**的——不写它，Vite 会把
         * Upgrade 请求当成普通 HTTP 打过去，握手直接失败（表现为"实时通道一直重连"）。
         * 生产环境由 nginx 反代承担同一件事（§62），前端代码里没有任何端口。
         */
        '/ws': { target: apiTarget, ws: true, changeOrigin: true },
        '/healthz': { target: apiTarget, changeOrigin: true },
        '/readyz': { target: apiTarget, changeOrigin: true },
      },
    },
  }
})
