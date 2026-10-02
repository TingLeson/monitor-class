/// <reference types="vite/client" />

/**
 * 只声明本 app 真正读取的环境变量。
 *
 * WHY 不写成 `[key: string]: any`：拼错变量名（VITE_API_BASE_URL → VITE_API_URL）
 * 会静默变成 undefined，最后表现为"请求打到自己 dev server 上 404"，
 * 而在类型层面直接报错能省掉一次半小时的排查。
 */
interface ImportMetaEnv {
  /** API 根地址。留空 = 同源（开发走 Vite 代理，生产走 nginx 反代，§62）。 */
  readonly VITE_API_BASE_URL?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
