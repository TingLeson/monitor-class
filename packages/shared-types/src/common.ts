/**
 * 全局标量别名。
 *
 * 约定（§43 / §51）：HTTP JSON 一律 camelCase，数据库列名 snake_case 只存在于后端，
 * 前端不得直接搬运数据库列名，避免两套命名在组件里混用。
 */

/** 后端主键（PostgreSQL UUID，§9–§12）。前端只把它当不透明字符串。 */
export type Uuid = string

/**
 * RFC 3339 时间戳字符串（Go `time.Time` 的 JSON 形式，例如 `2026-10-02T19:00:00Z`）。
 * 前端保留字符串形态、在展示层才格式化，避免 Date 的时区隐式转换。
 */
export type IsoDateTime = string
