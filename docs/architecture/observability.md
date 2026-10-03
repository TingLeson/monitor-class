# 可观测性与生产加固（Observability & Hardening）

> 本文档描述 **Phase 11 已实现**的服务端生产加固：指标（§59/§77）、结构化日志（§59）、
> 传输层加固（§63）、可信代理与真实客户端 IP 的推导规则、以及优雅关闭的时序（§62）。
>
> 一句话概括本 Phase 的成败标准（任务书 §77）：
>
> > **服务能不能被外部观察、能不能安全地对外、能不能在滚动重启时不制造故障。**
>
> 相关文档：[控制面](control-plane.md)、[架构总览](overview.md)、[实时事件流](realtime-flow.md)、
> [认证](../auth/authentication.md)、[RBAC](../auth/rbac.md)。
>
> 本文档只覆盖 `services/api`。反向代理（Caddy）的配置、容器编排与 `deploy/**` 由部署配置负责，
> 本文档在需要衔接处明确指出边界。

---

## 1. 指标总览（§59/§77）

所有指标以 `classwatch_` 前缀注册，暴露在 **`/metrics`**（与 `/healthz`、`/readyz` 同层）。
实现位于 `internal/metrics`，**没有引入任何 Prometheus 客户端依赖**（理由见 §2）。

| 指标 | 类型 | 标签 | 含义 | 埋点位置 |
| --- | --- | --- | --- | --- |
| `classwatch_http_requests_total` | counter | `method`、`route`、`status` | 已完成的 HTTP 请求数 | `httpapi.MetricsMiddleware` |
| `classwatch_http_request_duration_seconds` | histogram | `method`、`route`（桶 5ms…10s） | 处理器耗时（秒） | 同上 |
| `classwatch_http_requests_in_flight` | gauge | — | 当前正在处理的请求数（不含 `/metrics` 自身） | 同上 |
| `classwatch_ratelimit_rejected_total` | counter | `scope`、`dimension` | 被限流拒绝的请求数 | `httpapi.rateLimit`（唯一知道策略名的地方） |
| `classwatch_ws_connections` | gauge | `role` | 本进程存活的业务 WebSocket 连接数 | `realtime.Hub.register/unregister` |
| `classwatch_ws_messages_total` | counter | `direction`、`type` | 业务消息条数（in=客户端发来，out=已写出） | `realtime.conn.readPump/writePump` |
| `classwatch_ws_slow_consumer_disconnects_total` | counter | — | 因写队列满而被断开的连接数（§47 的背压决策） | `realtime.conn.enqueue` |
| `classwatch_session_status` | gauge | `status` | `student_sessions` 按状态的普查（六档，**周期刷新**） | `cmd/api` 采样循环 + `session.Postgres.CountByStatus` |
| `classwatch_webhook_events_total` | counter | `event`、`result` | LiveKit webhook 投递结果 | `session.Processor.ProcessWebhook`（applied/ignored/rejected）+ `httpapi.webhookHandler`（invalid_signature） |
| `classwatch_media_calls_total` | counter | `operation`、`result` | 媒体面 RoomService 调用次数 | `media.Client` 的公开方法 |
| `classwatch_media_call_duration_seconds` | histogram | `operation`（桶 10ms…10s） | 媒体面调用耗时（秒） | 同上 |
| `classwatch_private_talk_active` | gauge | — | 本进程当前活跃的私密讲话数（按课堂计数） | `session.Service`（派生自状态机注册表） |
| `classwatch_db_pool_connections` | gauge | `state` | pgxpool 连接数（acquired/idle/total） | `cmd/api` 采样循环（`pool.Stat()`） |
| `classwatch_build_info` | gauge | `version`、`commit` | 恒为 1 的构建标识，用于把行为变化对齐到一次发布 | `cmd/api` 启动时 |

### 1.1 为什么这样切分（而不是更多/更少）

- **按"谁拥有事实"埋点，而不是按"哪个端点调用它"**：媒体指标埋在 `media.Client`，因为控制面
  对 LiveKit 的每一次调用都经过它；埋在 `session` 层只能覆盖学生会话端点，会漏掉关课、
  webhook 和未来的调用方——**部分覆盖的指标比没有指标更危险**，因为它看起来是完整的。
- **`status` 只用于聚合数与错误率，不用来区分业务分支**：业务分支已经在 `error.code` 里，
  前端按 code 分支（§58）。指标只需要回答"5xx 有多少"。
- **`scope`/`dimension` 而不是 `route`/`key`**：限流策略名（`login`/`api`/`private_talk`…）
  是有限集合，而 key 里含有客户端 IP 与账号名（见 §4）。
- **`direction`/`type`（WS）而不是 `user_id`**：消息量按方向与类型就能回答"是不是 PING 洪水"、
  "老师端有没有收到推送"，而按用户切会立刻产生基数爆炸。
- **`classwatch_private_talk_active` 是"派生值"**：它从私密讲话状态机的注册表读出，不是靠
  `+1/-1` 维护。少一条路径就会漂移，而"指标说有一场私密讲话，实际没有"会让值班的人去追一个
  不存在的课堂。

### 1.2 会话状态普查为什么是"周期刷新"

`classwatch_session_status` 每 `METRICS_SESSION_REFRESH_INTERVAL`（默认 15s）执行一次

```sql
SELECT status, count(*) FROM student_sessions GROUP BY status
```

并**把六个状态全部写一遍**（没有行的写 0）。理由：

- 这是运维问题（"现在各状态各有多少"），不是每请求问题。放进状态机热路径等于给每次状态迁移
  加一次数据库往返；
- 每次刷新都写全部六档，避免了"只在非零时设置"导致的**只会上升的 gauge**——一个状态从 3 变 0
  却永远停在 3，是最容易误导值班的指标缺陷；
- 采样失败时**保留上一次的值**并打 warn，而不是清零：清零看起来像"所有学生都离开了"。

`/metrics` 本身**不计入** `classwatch_http_requests_total`：否则计数器会以抓取频率自增，
容量规划与 QPS 面板都会被自己的抓取污染。

---

## 2. 为什么手写 exposition，而不引 Prometheus 客户端

任务书 §77 要的是**指标**，不是一个指标生态。本项目的指标集合是有限的、明确列举的十几个族，
而官方客户端会带来：一棵依赖树（其中一部分是不可裁剪的）、以及"什么都要收集"的默认倾向
（进程/Go runtime 收集器、第二套 HTTP 暴露面）。

Prometheus 文本格式本身很小且稳定：

```text
# HELP name help text
# TYPE name counter
name{label="value"} 123
```

于是 `internal/metrics` 自己实现：

| 需求 | 实现 | 位置 |
| --- | --- | --- |
| 注册表 + 命名/标签名校验 | `Registry`（重复注册、非法名、保留标签 `le` 直接 panic） | `metrics.go` |
| counter / gauge / histogram | `Counter`/`Gauge`/`Histogram`（原子操作，句柄可缓存） | `metrics.go` |
| 文本格式转义 | `escapeHelp`（`\`、换行）、`escapeLabelValue`（`\`、`"`、换行） | `metrics.go` |
| `+Inf` / `NaN` / 浮点格式 | `formatFloat` | `metrics.go` |
| 直方图分桶 | 非累积计数 + 渲染时累积，末桶即 `+Inf` | `metrics.go` |
| 族内样本排序、族按名字分组 | `family.snapshot` + `WriteExposition` | `metrics.go` |

**语法正确性由单元测试保证**（`internal/metrics/metrics_test.go`）：转义、`+Inf`、分桶边界、
并发递增、族连续且按名字排序、空族仍然输出 `HELP/TYPE`。

### 2.1 热路径的开销设计

每一个指标族只在注册时创建一次；调用点在**第一次**用 `With(...)`/`HTTPRoute(...)` 解析出句柄并
缓存。之后记录一次样本是**一次原子加**：不加锁、不查 map、不分配内存。

- HTTP 的 `route` 标签缓存是 `httpapi.MetricsMiddleware` 内的一张 `map[method+route]`，
  键是可比较结构体（不装箱、不拼字符串）；
- `status` 标签在 handler 跑完后才知道，所以每个 route 句柄持有一个**按状态码索引的定长数组**
  （`[600]atomic.Uint64`），记录时只做一次数组索引 + 原子加。HTTP 状态码是三位数字、集合有界，
  数组既比 map 快，也顺手保证了"不可能出现非状态码的标签值"；
- 直方图的 `sum` 用 float64 位模式的 CAS 循环更新，而不是互斥锁：HTTP 时延直方图是每个请求都会
  写的东西，一把锁会把它变成全局争用点。

实测（Apple M4 Pro，`go test -bench`）：

```text
BenchmarkRouteInstrumentsObserve-14    4.133 ns/op    0 B/op    0 allocs/op
BenchmarkRequestWithoutMetrics-14       1439 ns/op   1886 B/op  37 allocs/op
BenchmarkRequestWithMetrics-14          1430 ns/op   1886 B/op  37 allocs/op
```

即：埋点本身**零分配**，整链路的增量落在测量噪声里（`TestMiddlewareOverheadIsBounded` 用它作为
回归护栏）。

---

## 3. 基数控制：哪些值**绝不**进标签

标签值的基数决定了 Prometheus 的内存与查询代价。本项目的规则是"**标签值必须是有限集合，且由
代码或路由表决定**"：

| 绝不作为标签 | 原因 | 替代做法 |
| --- | --- | --- |
| `user_id`、`session_id`、`classroom_id`、`run_id` | 无界且是个人标识 | 只进日志（§59 允许的关联字段） |
| 客户端 IP、账号名 | 攻击者可无限制造；账号名指向具体的人 | 限流指标只用 `scope`+`dimension`（`ip`/`account` 说明"key 是什么做的"） |
| **真实请求路径** | 一个课堂一个 UUID，扫描器一个路径一个值 | 用 Gin 的**路由模板**（`c.FullPath()`），未匹配记为 `unmatched` |
| 客户端自选的 HTTP method | `curl -X <随机>` 即可无限制造 | 白名单方法保留原名，其余折叠为 `other` |
| 客户端自选的 WS 消息 `type` | `{"type":"<随机>"}` 即可无限制造 | 只保留协议里真实存在的类型，其余折叠为 `other`/`unparseable` |
| 未验签 webhook 的 `event` 名 | 请求体不可信，等于让攻击者写标签 | 记为常量 `event="unknown"` |
| 房间名、track sid、identity | 每课堂/每 track 一个新值 | 只在日志与 `session_events` 里出现 |
| 错误文本、SQL、URL 片段 | 无界，且可能包含敏感内容 | 错误只进日志；指标只有 `result="error"` |

`event` 标签是唯一来自外部系统（LiveKit）的标签：它经过**验签**后才被采用，并且
`webhookEventLabel()` 仍会把它限制成"短、小写、下划线"的形态，否则折叠为 `unknown`。

---

## 4. 日志（§59）

### 4.1 形态与配置

| 变量 | 取值 | 说明 |
| --- | --- | --- |
| `LOG_LEVEL` | `debug`/`info`/`warn`/`error` | 级别 |
| `LOG_FORMAT` | `text`/`json`/空 | 空 = 跟随 `APP_ENV`（生产 JSON，其余 text）。staging 想验证生产日志管道时显式设 `json` |

JSON 模式**一行一条记录**（`slog.NewJSONHandler`），字段名是稳定的：

```text
request_id  user_id  role  classroom_id  run_id  session_id  event  action
method path status duration_ms remote_ip response_bytes
livekit_event webhook_event_id room close_code cause…
```

- 访问日志：每个请求一行，`path` 是**路径**（不含查询串），另有一个 `query` 字段是**过滤过**的
  查询串（`logging.SanitizeQuery`）；
- 4xx 记 `info`（可预期的业务结果），5xx 记 `error`，健康探针降到 `debug`；
- 每个请求的日志都带 `request_id`（`X-Request-Id` 透传或新生成、并回写响应头）；
- panic 恢复日志带 `stack`，**响应体只有统一的错误信封**，不含 panic 值（§58/§63）。

### 4.2 敏感值的三道防线

1. **约定**：`internal/infrastructure/logging` 的包注释写明禁止记录密码、Cookie、完整 token、
   LiveKit secret（§59）。
2. **RedactingHandler（执行层）**：`logging.New` 构建的每个 handler 都被包一层过滤器，
   **字段名**命中 `password/passwd/pwd/secret/token/cookie/authorization/credential/bearer/jwt/
   apikey/privatekey/signature/dsn/databaseurl/connectionstring` 时，值一律替换为
   `<redacted>`；`slog.Group` 会递归下沉，不丢组内的非敏感字段。
   - 有意"过度匹配"：`token_ttl` 这类字段也会被抹掉，代价是少量排障信息；
     漏掉 `livekit_token` 的代价是凭据永久留在日志里。
   - 它**不能**阻止被拼进 message 的秘密（`log.Info("failed for "+password)`），
     这正是 §59 同时要求调用方自律、且禁止整包记录 body 的原因。
3. **查询串过滤**：`logging.SanitizeQuery` 保留参数名、抹掉敏感参数值（`token`、`code`、
   `session_id`、`api_key`…），并把长度截断在 512 字节。WebSocket 与媒体 token 严禁放在 URL
   （§47）；这层是"万一还是有人放了"的兜底。

对应的测试在 `internal/infrastructure/logging/logger_test.go`：真实值**不得**出现在输出里、
`<redacted>` 必须出现、非敏感字段必须原样保留。

---

## 5. 传输层加固（§63）

| 项 | 行为 | 位置 |
| --- | --- | --- |
| 请求体上限 | `HTTP_MAX_BODY_BYTES`（默认 1 MiB）。声明超限 → 立刻 413；未声明长度（chunked）由 `http.MaxBytesReader` 兜底 | `httpapi.BodyLimitMiddleware` + 各端点的 `bindJSONLimit` |
| 413 错误码 | `PAYLOAD_TOO_LARGE`（新增，§58 扩展） | `apperr.codes.go` |
| 安全响应头 | `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` | `httpapi.SecurityHeadersMiddleware` |
| HSTS | **仅在判定为 HTTPS 时**下发（见下） | 同上 |
| 404 / 405 | 统一信封；代码为 `NOT_FOUND` / `METHOD_NOT_ALLOWED`（新增），405 带 `Allow` | `router.NoRoute/NoMethod` |
| 服务器超时 | `HTTP_READ_HEADER_TIMEOUT`/`HTTP_READ_TIMEOUT`/`HTTP_WRITE_TIMEOUT`/`HTTP_IDLE_TIMEOUT` 全部显式且可配 | `cmd/api` |
| 超时校验 | `READ_HEADER ≤ READ`（否则内层设置会被外层静默截断，配置在说谎） | `config.parseHTTPServer` |
| 关闭中拒新请求 | 排空期间新请求得到 **503 `SERVICE_UNAVAILABLE`** + `Retry-After` | `httpapi.DrainGate` |
| 排空窗口 | `HTTP_DRAIN_DELAY`（默认 2s）内**继续监听并只回 503**，之后才关闭 listener | `cmd/api` |
| 限流 | 登录（IP/账号）、`/api/v1` 粗粒度、私密讲话、媒体 token、join、WS 握手（IP）；窗口与阈值全部环境变量化 | `httpapi/ratelimit.go` |
| WS 并发上限 | 每客户端地址同时最多 `WS_MAX_CONNECTIONS_PER_IP` 个业务 socket | `httpapi.socketCap` |
| CSRF 覆盖 | 所有 POST/PUT/PATCH/DELETE 路由都在 CSRF 中间件之后注册；由**走真实路由表**的集成测试守护 | `TestEveryUnsafeRouteRequiresCSRF` |
| CORS | 仅回显白名单来源；不在白名单的来源：预检直接 403，非安全方法直接 403 | `httpapi.CORSMiddleware` |

### 5.1 HSTS 为什么只在 HTTPS 上下发

本地开发是 `http://localhost:8090`。浏览器一旦在明文 HTTP 响应上收到 HSTS，就会把该主机
**钉死**在 HTTPS 上（`max-age=31536000` = 一年），开发者除非手动清 HSTS 状态，否则再也打不开
本地页面。因此判定规则是：

```text
req.TLS != nil                                    → HTTPS
否则 直连对端 ∈ TRUSTED_PROXIES 且 X-Forwarded-Proto 最右跳 == https → HTTPS
否则                                               → 不下发 HSTS
```

注意 `X-Forwarded-Proto` **只在直连对端是已配置的可信代理时**才被相信：否则调用方就能决定
我们是否下发 HSTS。判定失败的方向是刻意选的——少一个 HSTS 是扫描器会发现的加固缺口，
多一个 HSTS 会把开发者锁在自己的机器外面。

### 5.2 WriteTimeout 会不会掐断 WebSocket

**不会**，并且这一点有真实 socket 的测试证明（`internal/realtime/timeout_test.go`）：

1. net/http 在 handler **hijack** 连接时会清空该连接的 deadline；
2. hub 自己为每一帧写设置 `WriteWait` 上限，不依赖服务器设置。

测试用 `WriteTimeout: 250ms`、`ReadTimeout: 1s` 的真实 `http.Server`，连接后**先等 1.5 秒**
（越过两个超时），再双向收发（客户端 PING→PONG、服务端推送 SCREEN_LOST），全部成功。
若未来 net/http 改变 hijack 语义（或有人把 hub 的自管 deadline 删掉），这条测试会立刻失败。

### 5.3 `/metrics` 的鉴权策略（为什么它不需要 session）

`/metrics` **不需要**会话鉴权：

- 它只包含**聚合数**：计数、耗时分布、连接数、状态普查。没有 user id、session id、课堂名、
  账号、token、请求体；
- 要求鉴权意味着**抓取器要持有一个人类的凭据**，这是比暴露聚合数更差的交换；
- 但它**不是**给公网看的：数字会泄露负载、版本与错误率。

生产建议（部署侧负责，`deploy/**` 不在本 Phase 范围内）：

1. **首选**：只在内部网络监听或由反向代理限制来源（`@metrics` matcher + `remote_ip` 限制）；
2. 或者加一层 `Authorization: Bearer <token>`（Caddy `basicauth`/`header` 指令即可），
   由 Prometheus 的 `authorization` 配置携带；
3. 无论哪种，都不要把 `/metrics` 暴露到与前端同源的公网路径上。

---

## 6. 可信代理与真实客户端 IP（§63）

限流 key、审计日志 `remote_ip`、`sessions.ip`、HSTS 判定**全部**使用同一个解析器
（`httpapi.ClientIPResolver`），因此它们不可能对"这个请求是谁发的"给出不同答案。

### 6.1 推导规则（从右往左）

```text
1. 取 TCP 对端 peer = RemoteAddr 的主机部分（去掉端口/方括号/zone；IPv4-mapped 归一化）
2. 若 peer ∉ TRUSTED_PROXIES（默认空 = 谁都不信）：
       返回 peer          ← X-Forwarded-For / X-Real-IP / X-Forwarded-Proto 一律忽略
3. 若 peer ∈ TRUSTED_PROXIES：
       a. 解析 X-Forwarded-For，从右往左扫描：
            跳过解析失败项（"unknown"）与本身可信的跳；
            第一个不可信地址即"代理实际看到的客户端" → 返回它
            若全部可信 → 返回最左项（能拿到的最好答案）
       b. 没有可用的 X-Forwarded-For 时，使用 X-Real-IP（同样是可信代理才有资格设置）
       c. 都没有 → 返回 peer
```

**为什么从右往左**：最右边的条目是**我们的代理**追加的。从左往右读会返回客户端自己塞进
header 的第一个值——这正是伪造向量的本体。测试用例（`internal/httpapi/clientip_test.go`、
`auth_test.go`）：

| 场景 | 期望 |
| --- | --- |
| 默认（`TRUSTED_PROXIES` 为空）+ 伪造 `X-Forwarded-For: 1.2.3.4` | 限流与日志都按真实对端 IP |
| 伪造头 + 轮换多个假 IP | **不会**获得新的限流桶（`TestSpoofedXForwardedForCannotBypassRateLimiting`） |
| 直连对端可信 + `X-Forwarded-For: <客户端>` | 采用该客户端（否则整间教室共用一个桶） |
| 可信代理 + 两层自己的代理链 | 跳过右侧可信跳，取第一个不可信地址 |
| 全链可信 | 取最左项 |
| 不可信对端 + `X-Real-IP` | 忽略 |
| `::ffff:10.0.0.5` 对端 + IPv4 规则 | 命中（双栈监听不会静默失去信任） |

### 6.2 为什么默认"谁都不信"

`TRUSTED_PROXIES` 默认空。若任何来源都能设置 `X-Forwarded-For`，一个攻击者就能：

- 每个请求换一个假 IP → 每个 IP 桶都是新的 → **绕过所有按 IP 的限流**；
- 污染 `sessions.ip` 与访问日志 → 事后审计得到的是攻击者编造的地址。

所以默认是安全的（只是"所有客户端共用一个桶"，这会在 `/readyz` 与限流日志里立刻暴露），
而生产必须在 `TRUSTED_PROXIES` 里填**真实代理网段**：填窄了共享桶，填宽了可伪造。

---

## 7. 优雅关闭时序（§62）

```mermaid
sequenceDiagram
    participant O as 编排器 / docker stop
    participant P as API 进程
    participant D as DrainGate
    participant S as http.Server
    participant H as WebSocket Hub
    participant X as pgxpool / Redis

    O->>P: SIGTERM
    P->>P: signal.NotifyContext 取消 ctx
    P->>D: BeginDraining()
    Note over D: 此后到达的请求（keep-alive / HTTP2 上仍在途的）<br/>一律 503 SERVICE_UNAVAILABLE + Retry-After<br/>例外：/healthz（活跃探针）与 /metrics（最后一次抓取）
    P->>P: 继续服务 HTTP_DRAIN_DELAY（默认 2s）
    Note over P: 这是负载均衡器把本实例摘除的窗口；<br/>也是"已建立的连接拿到 503 而不是被 reset"的窗口
    P->>S: Shutdown(ctx, HTTP_SHUTDOWN_TIMEOUT)
    Note over S: 关闭 listener（新 TCP 连接被拒）<br/>等待在途请求完成
    S-->>P: 排空完成（或超时 → 记 error，仍继续下一步）
    P->>P: 等待 HTTP_DRAIN_DELAY 结束
    P->>P: stopSampler()（避免采样与连接池拆除竞争）
    P->>H: Hub.Shutdown(ctx, HTTP_SHUTDOWN_TIMEOUT)
    Note over H: 置 closing=true，拒新握手<br/>对每个连接发 "going away" close frame<br/>等待写泵退出
    P->>X: 关闭 Redis、pgxpool（defer，逆序）
    P-->>O: 退出码 0
```

要点：

- **先开 drain gate，再等一个 drain delay，最后才 Shutdown**。`http.Server.Shutdown` 立刻关闭
  listener，并且会**静默丢弃**一个到达"它已经决定退休的连接"上的请求——客户端完全无法与网络故障
  区分。所以顺序必须是：开闸（新请求 503）→ 继续监听 `HTTP_DRAIN_DELAY`（默认 2s，足够一次
  readiness 探针周期把本实例摘除）→ 关 listener。
  - 这个 `drain delay` 不是从文档里抄来的：把服务真跑起来发一次 SIGTERM 才发现，**不加这一
    步，排空期间的请求根本到不了中间件**（net/http 在 Shutdown 状态下直接关连接）。真实链路
    观测到的结果见 §9。
  - `HTTP_DRAIN_DELAY=0` 表示"立即停止监听"，只有在没有任何流量路由到本进程时才正确
    （单实例部署或测试）。
- **探针的例外是刻意的**：`/healthz` 与 `/metrics` 在排空期间仍然可用。liveness 在滚动发布时
  失败会让编排器发 SIGKILL，把一个优雅发布变成硬杀；`/readyz` **不**例外——503 正是告诉负载
  均衡器"别再往这里路由"的信号。
- **每一步都有自己的 deadline**，总预算约为 `3 × HTTP_SHUTDOWN_TIMEOUT`，仍远低于编排器默认
  30s 的 SIGKILL 宽限期。
- **WebSocket 必须显式关闭**：连接已被 hijack，`http.Server.Shutdown` 不等它们；不发
  "going away" 帧的话，浏览器要等到下一次 PING 超时才意识到断线，并把一次发布当成网络故障
  （§47 的前端重连+回读快照契约依赖这个帧）。
- 全过程有结构化日志：`shutdown signal received` → `http drain complete` → `ws_shutdown` →
  `shutdown complete`，都带 `elapsed_ms`。

对应测试：`TestDrainGateRefusesNewRequestsWith503`（503、`Retry-After`、探针例外、幂等），
以及既有的 hub 关闭测试（§47）。

---

## 8. 告警建议（PromQL）

阈值都是**起点**，不是真理：先按"这条规则响了，我会不会真的去看"筛一遍，再按实际流量调整。

| 告警 | PromQL | 阈值理由 |
| --- | --- | --- |
| 5xx 比例 | `sum(rate(classwatch_http_requests_total{status=~"5.."}[5m])) / sum(rate(classwatch_http_requests_total[5m])) > 0.01` | 1% 的请求失败对"上课中"的产品已经不可接受；持续 5m 才响，避免单次发布抖动 |
| 单路由 p95 变慢 | `histogram_quantile(0.95, sum(rate(classwatch_http_request_duration_seconds_bucket[5m])) by (le, route)) > 1` | 控制面调用长期超过 1s 时，老师端的"监控墙"体感已经明显滞后 |
| 媒体面失败 | `sum(rate(classwatch_media_calls_total{result="error"}[5m])) by (operation) > 0.1` | 每秒 0.1 次失败说明 LiveKit 或凭据有问题；`operation` 分组直接指向 ensure_room / update_subscriptions |
| 限流拒绝激增 | `sum(rate(classwatch_ratelimit_rejected_total[5m])) by (scope, dimension) > 1` | 稳态几乎是 0；`scope=login` 涨说明有人在爆破，`scope=api` 涨说明有客户端失控或代理配置错了 |
| webhook 验签失败 | `sum(increase(classwatch_webhook_events_total{result="invalid_signature"}[10m])) > 0` | 任何一条都值得看：要么密钥不一致（事件在被丢弃），要么有人在伪造 |
| webhook 应用失败 | `sum(rate(classwatch_webhook_events_total{result="rejected"}[5m])) > 0` | 意味着 LiveKit 的观察没写进控制面，老师的墙会与现实不一致 |
| 慢消费者掉线 | `sum(rate(classwatch_ws_slow_consumer_disconnects_total[5m])) > 0.1` | 掉线会触发前端重连+回读快照；持续掉线说明网络或某类客户端有问题 |
| 会话卡在 CONNECTING | `sum(classwatch_session_status{status="CONNECTING"}) > 20 and sum(rate(classwatch_http_requests_total{route="/api/v1/student/classrooms/:id/join",status="200"}[5m])) == 0` | 拿到 token 但从没被媒体面观察到：通常是媒体面不可达或前端在屏幕门卡住 |
| 连接池饱和 | `classwatch_db_pool_connections{state="acquired"} / on() classwatch_db_pool_connections{state="total"} > 0.9` 持续 5m | 池满意味着请求在排队；配合 p95 一起看能区分"数据库慢"与"泄漏了连接" |
| 私密讲话长时间活跃 | `max_over_time(classwatch_private_talk_active[2h]) > 0` | 一场私密讲话持续两小时几乎一定是"忘记停止"或状态机卡住（§31） |
| 并发请求异常 | `max_over_time(classwatch_http_requests_in_flight[5m]) > 200` | 进程内并发是"一切变慢"的先行指标；具体阈值按实例规格定 |

面板建议按三层组织：**入口**（请求量/错误率/时延/限流）、**业务**（会话状态普查、私密讲话、
慢消费者）、**依赖**（媒体面调用与耗时、连接池）。

---

## 9. 怎么自己验证

```bash
cd services/api
# 全部 14 个指标族（HELP/TYPE 一定存在，即使还没有样本）
curl -s localhost:8100/metrics | grep -c '^# HELP'
# 真实流量之后的计数与直方图
curl -s localhost:8100/metrics | grep -E 'classwatch_(http_requests_total|http_request_duration_seconds_count|media_calls_total|session_status)'
# 413 / 405 / 404
curl -si -X POST localhost:8100/healthz | head -3
curl -s -X POST localhost:8100/api/v1/teacher/auth/login -H 'Content-Type: application/json' \
     --data-binary @<(python3 -c "print('{\"account\":\"' + 'x'*1200000 + '\"}')") | head -c 120
# SIGTERM：排空窗口内 /healthz=200、/metrics=200、/readyz=503、/api/v1/meta=503
kill -TERM <pid>

gofmt -l . && go build ./... && go vet ./... && go test -count=1 ./...
TEST_DATABASE_URL='postgres://classwatch:classwatch_dev_password@localhost:5432/classwatch_test?sslmode=disable' \
  go test -p 1 -count=1 ./...
go test -race ./internal/...
go test -run XXX -bench 'BenchmarkRequest|BenchmarkRouteInstruments' -benchmem ./internal/httpapi/

# 真实链路
API_ADDR=:8100 go run ./cmd/api
curl -s localhost:8100/metrics | head -40          # 全部指标族 + HELP/TYPE
curl -s localhost:8100/metrics | grep ws_connections
curl -si localhost:8100/definitely-not-a-route     # 404 + NOT_FOUND
```

真实链路的观测结果（Phase 11 验收时摘录）：

```text
$ curl -s localhost:8100/metrics | grep -c '^# HELP'
14
$ curl -s localhost:8100/metrics | grep -E 'classwatch_(media_calls_total|session_status\{status="CONNECTING)'
classwatch_media_calls_total{operation="ensure_room",result="ok"} 1
classwatch_media_calls_total{operation="sign_token",result="ok"} 1
classwatch_session_status{status="CONNECTING"} 1
$ curl -si -X POST localhost:8100/healthz | head -1
HTTP/1.1 405 Method Not Allowed
$ <1.2 MiB body> → HTTP 413 {"error":{"code":"PAYLOAD_TOO_LARGE",...}}

# SIGTERM 之后，在同一条已建立的连接上：
before SIGTERM:  /healthz 200  /readyz 200  /api/v1/meta 200
after  SIGTERM:  /healthz 200  /metrics 200  /readyz 503  /api/v1/meta 503
```

`docs/architecture/observability.md`（本文）是这些行为的**契约**：改动 `/metrics` 的族名、
标签集或关闭语义时，必须同时改这里与对应的测试。
