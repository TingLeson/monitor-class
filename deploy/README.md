# ClassWatch 生产部署（Phase 11）

> 这份 README 是**上线操作手册**：从一台空服务器到"第一个真实课堂跑通"。
> 网络原理、TURN 验证、多网络测试矩阵、资源估算、故障排查表在
> [`docs/development/deployment.md`](../docs/development/deployment.md)；
> 本地开发环境在 [`docs/development/setup.md`](../docs/development/setup.md)。
>
> 任务书依据：§62（Production 拓扑）、§63（Security Checklist）、§77（Phase 11）。

---

## 0. 这个目录里有什么

| 文件                                   | 作用                                                        | 什么时候会改它                 |
| -------------------------------------- | ----------------------------------------------------------- | ------------------------------ |
| `Caddyfile`                            | 多站点反向代理 / 自动 HTTPS / 安全响应头 / `/ws/*` 升级     | 加域名、改 CSP、换媒体面方案时 |
| `docker-compose.prod.yml`              | 生产编排（网络分段、资源限制、日志轮转、健康检查）          | 改镜像、调资源、加服务时       |
| `.env.production.example`              | 生产环境变量模板（**只有占位值**）                          | 新增配置项时                   |
| `docker/web.Dockerfile`                | 三个 SPA 的生产构建镜像                                     | 升级 Node/pnpm 时              |
| `docker/publish-web.sh`                | 一次性容器把静态产物发布到卷                                | 一般不动                       |
| `scripts/net-check.sh`                 | 网络可达性 / 证书 / WSS / Twirp / UDP-STUN 检查             | 一般不动                       |
| `scripts/backup-db.sh`                 | `pg_dump -Fc` 备份 + 保留 N 份 + 元数据                     | 改备份策略时                   |
| `scripts/restore-db.sh`                | 恢复（含版本/校验和/活动连接三道闸门）                      | 一般不动                       |
| `docker/README.md` · `nginx/README.md` | 为什么 Dockerfile 不在 deploy/、为什么选 Caddy 而不是 nginx | 方案变更时                     |

**只有 `caddy` 一个服务对外暴露端口。** 其余全部在内网：`api` 不发布端口，
`postgres`/`redis` 连网关都没有（`internal: true`）。

```text
Internet ──► Caddy(:80/:443) ──┬─► student./teacher./admin.  静态产物 + 同源反代 /api /ws
                               ├─► api.<domain>              API + /ws + LiveKit webhook
                               └─► rtc.<domain>              仅自建 LiveKit 时需要
                                        │
                                        ├─► api:8080 ─► postgres:5432 / redis:6379（内部网络）
                                        └─► LiveKit Cloud（WSS / WebRTC / TURN-TLS，出网）
```

---

## 1. 上线 10 步

每一步都给出**验证命令**。部署的危险不在于"命令跑不起来"（那还算好的），
而在于"命令跑起来了，但行为不是你以为的那样"——所以每一步都必须验证。

### 步骤 1 — DNS：四条 A/AAAA 记录指向服务器

```text
student.<domain>   A/AAAA  <服务器公网 IP>
teacher.<domain>   A/AAAA  <服务器公网 IP>
admin.<domain>     A/AAAA  <服务器公网 IP>
api.<domain>       A/AAAA  <服务器公网 IP>
# rtc.<domain> 只有自建 LiveKit 时才需要；用 LiveKit Cloud 时**不要**建这条记录
```

- **TTL 先设 300**：出问题时你能在 5 分钟内把流量切走；稳定运行一段时间后再调大。
- 验证：`dig +short student.<domain> A` 必须等于服务器 IP（**四条都要查**）。
- 为什么必须四条都通：Caddy 一次启动就会为四个域名申请证书，
  任何一条 DNS 没生效都会让那次 ACME 申请失败并进入退避重试（表现为"某个域名打不开"）。

### 步骤 2 — 服务器准备

| 项     | 建议                                      | 原因                                                |
| ------ | ----------------------------------------- | --------------------------------------------------- |
| 系统   | Ubuntu 22.04+ / Debian 12+                | 与 Docker 官方仓库配合最顺                          |
| 规格   | 2 vCPU / 4 GB 内存 / 40 GB 盘（起步）     | 1 老师 + 20~30 学生的量级，估算依据见 deployment.md |
| Docker | `docker-ce` + `docker compose` 插件（v2） | `deploy.*` 资源限制需要 Compose v2                  |
| 用户   | 非 root 用户加入 `docker` 组              | 少一个"所有文件都 root 属主"的坑                    |
| 时间   | 打开 NTP（`timedatectl`）                 | TLS 证书校验、TOTP、日志时间线都依赖时钟            |

```bash
# 代码就位（二选一）
git clone <repo> /srv/classwatch && cd /srv/classwatch && git checkout phase/11-production
# 或用 CI 产物 rsync；关键是 deploy/ 与 services/ 必须来自**同一个** commit

# 关键：环境文件先建好并锁权限
cd /srv/classwatch
cp deploy/.env.production.example deploy/.env.production
chmod 600 deploy/.env.production
$EDITOR deploy/.env.production       # 见步骤 4
```

> ✅ **`deploy/.env.production` 已经在 `.gitignore` 里**（已核实：`git check-ignore -v deploy/.env.production`
> 会命中它）。这一行不能删：该文件含真实域名与密钥，而 `deploy/.env.production.example` 才是唯一该入库的版本。
> 习惯动作：提交前跑一次 `git status --porcelain | grep env.production`，确认它没有出现。

### 步骤 3 — 防火墙：只开该开的端口

| 端口        | 协议      | 对谁开放                     | 为什么                                               |
| ----------- | --------- | ---------------------------- | ---------------------------------------------------- |
| 22          | TCP       | 仅运维 IP                    | SSH                                                  |
| 80          | TCP       | 所有人                       | ACME HTTP-01 挑战 + HTTP→HTTPS 跳转                  |
| 443         | TCP       | 所有人                       | 全部业务（HTTPS 与 WSS）                             |
| 443         | UDP       | 所有人（可选）               | HTTP/3。不允许时删掉 compose 里的 `443:443/udp` 即可 |
| 50000-60000 | UDP       | 所有人（**仅自建 LiveKit**） | RTC 媒体包。用 LiveKit Cloud 时**不需要**在本机开放  |
| 7881        | TCP       | 所有人（**仅自建 LiveKit**） | RTC over TCP 兜底                                    |
| 5349 / 3478 | TCP / UDP | 所有人（**仅自建 TURN**）    | TURN over TLS / STUN                                 |

**绝对不要开放** `5432`（PostgreSQL）、`6379`（Redis）、`8080`（API）。
这份编排里它们根本没有发布端口；如果某次排障你临时加了 `ports:`，请立刻删掉并重启——
公网上的自动化扫描器发现一个开放数据库只需要几分钟。

```bash
# 云安全组之外再加一层主机防火墙（两道门，防止其中一道被误改）
sudo ufw default deny incoming && sudo ufw allow 22/tcp
sudo ufw allow 80/tcp && sudo ufw allow 443/tcp && sudo ufw allow 443/udp
sudo ufw enable && sudo ufw status verbose
```

### 步骤 4 — 填好 `.env.production`

必须替换的项（其余保持默认即可）：

| 变量                                                                         | 说明                                                                        |
| ---------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| `STUDENT_HOST` / `TEACHER_HOST` / `ADMIN_HOST` / `API_HOST`                  | 步骤 1 的域名                                                               |
| `ACME_EMAIL`                                                                 | **真实可收信**的邮箱（证书到期提醒）                                        |
| `API_IMAGE` / `WEB_IMAGE` / `APP_VERSION` / `APP_COMMIT`                     | 不可变镜像标签（不要 latest）                                               |
| `POSTGRES_PASSWORD` / `REDIS_PASSWORD`                                       | 用 `openssl rand` 生成，避免 URL 保留字符                                   |
| `CORS_ALLOWED_ORIGINS`                                                       | 三个真实 https 来源                                                         |
| `LIVEKIT_URL` / `LIVEKIT_API_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | LiveKit Cloud 或自建                                                        |
| `CADDY_APP_IP`                                                               | 默认 `172.31.0.2`，与 compose 里 app 网段一致（`TRUSTED_PROXIES` 由它派生） |

校验配置（**不启动任何容器**）：

```bash
docker compose --env-file deploy/.env.production -f deploy/docker-compose.prod.yml config >/dev/null && echo "配置解析通过"
```

### 步骤 5 — 拉起：先产物、再迁移、最后服务

```bash
docker compose --env-file deploy/.env.production -f deploy/docker-compose.prod.yml up -d --build
docker compose --env-file deploy/.env.production -f deploy/docker-compose.prod.yml ps -a
```

预期顺序（**这个顺序本身就是设计的一部分**）：

```text
web-dist （一次性，Exited 0）  ─┐
migrate  （一次性，Exited 0）  ─┼─► api (healthy) ─► caddy (running)
postgres (healthy) ─ redis(healthy) ─┘
```

- `web-dist` 退出码非 0 → **caddy 不会启动**。这是故意的：宁可站点起不来，
  也不要对外提供一个空白页面。
- `api` 的健康检查用 `/readyz`：它连不上 PostgreSQL/Redis/LiveKit 时 caddy 不会启动，
  避免把 5xx 直接提供给课堂。
- 首次启动 Caddy 会在几十秒内签发四个证书（日志里能看到 ACME 过程）。

```bash
# 看证书是否签发成功
docker compose --env-file deploy/.env.production -f deploy/docker-compose.prod.yml logs caddy | grep -i -E "certificate|acme|error"
```

### 步骤 6 — 迁移与第一个管理员

`migrate` 已经在步骤 5 自动跑完（一次性服务）。确认状态：

```bash
COMPOSE="docker compose --env-file deploy/.env.production -f deploy/docker-compose.prod.yml"
$COMPOSE run --rm migrate status
```

创建第一个管理员（密码只从 stdin 传入，不进 shell history / `ps` / 日志）：

```bash
$COMPOSE --profile tools run --rm -T adminctl \
  create-user --account admin --display-name "系统管理员" --role ADMIN --password-stdin
# 回车后粘贴密码，再按 Ctrl-D
```

> 学生**没有密码**（业务规则 §2.2），老师/管理员用 `create-user --role TEACHER`。

### 步骤 7 — 健康检查：脚本 + 探针 + 证书

```bash
deploy/scripts/net-check.sh api.<domain> <project>.livekit.cloud
```

它会把 DNS、TLS 证书有效期、HTTP→HTTPS 跳转、`/healthz`、`/readyz`（连 JSON 一起打印）、
`/ws/*` 的 101 握手、LiveKit 的 Twirp/WSS 端点、UDP-STUN 出站逐项列出来，
**有任何 FAIL 就以退出码 1 结束**（可以放进 CI 的发布后检查）。

另外手工确认一次业务层：

```bash
curl -sS https://api.<domain>/readyz | head -c 300     # 期望三个依赖都 ok
curl -sS -o /dev/null -w '%{http_code}\n' https://student.<domain>/   # 期望 200
```

### 步骤 8 — 打第一个真实课堂（在**两个不同网络**上）

这一步不能省：网络配置的错误在白名单里是看不出来的（§62）。

1. 老师端（网络 A，例如办公室 Wi-Fi）登录 → 建课堂 → 开课。
2. 学生端 1（网络 B，例如手机热点）登录 → 进入课堂 → 勾选整屏共享。
3. 老师端应看到缩略图；在**另一个浏览器标签**打开 `chrome://webrtc-internals`，
   按 [`deployment.md` 的 TURN 章节](../docs/development/deployment.md)确认候选类型。
4. 断掉学生 1 的网络 10 秒再恢复：老师端应显示掉线并在恢复后重新出现
   （这条同时验证了 `/ws/*` 没有被代理层降级）。
5. 学生 2 用**受限网络**（只允许 443 出站）重复一次：必须仍然能看到画面，
   否则说明 TURN/TLS 没有生效。

### 步骤 9 — 备份：先证明能恢复，再谈有没有备份

```bash
# 每日 03:30 备份，保留 7 份（示例）
crontab -e
30 3 * * * /srv/classwatch/deploy/scripts/backup-db.sh >> /var/log/classwatch-backup.log 2>&1
```

`backup-db.sh` 会写 `<时间戳>.dump` 与 `.dump.meta`（含 `schema_version`、PostgreSQL 版本、
sha256）。**必须**把这两个文件同步到另一台机器/另一个磁盘。

恢复演练（**上线前就做一次**，不要等出事）：

```bash
# 1) 在临时库上演练（不会碰生产库）
$COMPOSE exec -T postgres createdb -U classwatch classwatch_restore_drill
POSTGRES_DB=classwatch_restore_drill CONFIRM=restore deploy/scripts/restore-db.sh /var/backups/classwatch/<文件>.dump
$COMPOSE exec -T postgres dropdb -U classwatch classwatch_restore_drill
```

`restore-db.sh` 有三道闸门：sha256 校验、拒绝在目标库有活动连接时恢复、
恢复后校验 `schema_version` 与备份记录一致。

### 步骤 10 — 回滚

```bash
# 1) 改回上一个不可变标签（API 与前端可以分别回滚）
$EDITOR deploy/.env.production     # API_IMAGE / WEB_IMAGE / APP_VERSION
$COMPOSE up -d --no-deps api caddy
$COMPOSE exec -T api wget -qO- http://127.0.0.1:8080/readyz

# 2) 数据库：**不做** down migration
```

为什么数据库不跟着回滚：本项目的迁移遵循**向前兼容**原则（只加列/加表、
不删不改语义），所以"旧代码 + 新 schema"是可用状态，回滚代码即可。
只有在备份比当前镜像**新**的时候（`schema_version` 更大）才必须换回同版本镜像。

---

## 2. 日常运维速查

```bash
COMPOSE="docker compose --env-file deploy/.env.production -f deploy/docker-compose.prod.yml"

$COMPOSE ps                        # 状态与健康
$COMPOSE logs -f --tail=200 api    # 跟踪 API 日志（JSON）
$COMPOSE logs -f --tail=200 caddy  # 证书 / 反代错误
$COMPOSE exec -T api wget -qO- http://127.0.0.1:8080/metrics | head   # 指标（内网）
$COMPOSE exec -T postgres psql -U classwatch -d classwatch            # 数据库
deploy/scripts/net-check.sh api.<domain>                              # 网络与证书巡检
```

升级流程（顺序不要换）：

```text
1. CI 构建并推送不可变标签 → 2. 改 .env.production 的标签
3. $COMPOSE run --rm migrate status   （看清将要发生什么）
4. $COMPOSE up -d --build             （migrate 先跑，api 再换）
5. net-check + 打开教室走一遍         （验证）
6. 保留上一个标签至少一周             （回滚的抓手）
```

---

## 3. 常见故障（详细版见 deployment.md 的故障排查表）

| 症状                                   | 最可能的原因                                | 第一步                                                        |
| -------------------------------------- | ------------------------------------------- | ------------------------------------------------------------- |
| caddy 起不来，`web-dist` 退出码非 0    | 前端构建产物不完整（`index.html` 缺失）     | `$COMPOSE logs web-dist`                                      |
| 站点打开是白屏，控制台 404             | 静态资源缓存/产物不匹配                     | `$COMPOSE logs web-dist caddy`                                |
| 老师端"看不到学生画面"                 | TURN 未生效或 UDP 被封                      | deployment.md 的 TURN 章节 + `chrome://webrtc-internals`      |
| 实时状态不更新（点名/掉线）            | 反代没有转发 WebSocket Upgrade              | `net-check.sh` 第 5 节；检查 Caddyfile 的 `websocket_*`       |
| 大量 429                               | `TRUSTED_PROXIES` 没配 → 全站共用一个限流桶 | 检查 `.env.production` 的 `CADDY_APP_IP`/`TRUSTED_PROXIES`    |
| 登录后立刻掉线                         | Cookie `Secure` + 访问的不是 HTTPS          | 确认走的是 `https://`，且没有中间设备做 TLS 卸载              |
| `/readyz` 里 `livekit` 不是 ok         | key/secret 不匹配或出网被拦                 | `$COMPOSE exec -T api wget -qO- http://127.0.0.1:8080/readyz` |
| `up` 报 `Pool overlaps with other one` | `172.31.0.0/24` 与服务器上已有网络冲突      | 改 compose 里的网段 **并同步** 改 `CADDY_APP_IP`              |

---

## 3.1 手工校验 Caddyfile 时必须带上环境变量

否则 `{$ACME_EMAIL}` 展开为空，`email` 变成没有参数的空指令，Caddy 会报一句很难懂的
`parsing caddyfile tokens for 'email': wrong argument count or unexpected line ending`：

```bash
set -a; . deploy/.env.production; set +a
docker run --rm -e STUDENT_HOST -e TEACHER_HOST -e ADMIN_HOST -e API_HOST \
  -e ACME_EMAIL -e HTTP_MAX_BODY_BYTES \
  -v "$PWD/deploy/Caddyfile:/etc/caddy/Caddyfile:ro" \
  caddy:2.10-alpine caddy validate --config /etc/caddy/Caddyfile
# → Valid configuration
```

走 `docker compose -f deploy/docker-compose.prod.yml up` 时不会遇到这个问题：
compose 里的 `ACME_EMAIL: ${ACME_EMAIL:?...}` 会**先**拦住并给出中文提示，比 Caddy 的解析错好懂得多。

## 4. 需要在根目录 / Makefile 补的东西（**部署面之外，留给主分支改动**）

这份 Phase 11 只允许改 `deploy/**` 与 `docs/development/deployment.md`，
下面两项在部署面之外：

**（1）`.gitignore`——防止生产密钥被提交 ✅ 已完成**

```gitignore
# 生产环境变量（含真实域名与密钥），模板 .env.production.example 可以提交
deploy/.env.production
```

根 `.gitignore` 里**已经有这一行**（`git check-ignore -v deploy/.env.production` 可验证）。
它是"部署时手一抖 `git add -A` 就把生产密钥提交上去"的唯一一道门，不要删。

**（2）`Makefile`——把生产入口收拢成目标 ✅ 已添加**

（下面是已落到根 `Makefile` 的定义，`make help` 里可看到 `prod-*` 目标）：

```make
PROD_COMPOSE := docker compose --env-file deploy/.env.production -f deploy/docker-compose.prod.yml

prod-config: ## 校验生产配置（不启动容器）
	$(PROD_COMPOSE) config >/dev/null && echo "生产配置解析通过"

prod-up: ## 拉起/更新生产栈（含构建）
	$(PROD_COMPOSE) up -d --build

prod-ps: ## 生产容器状态
	$(PROD_COMPOSE) ps

prod-logs: ## 生产日志：make prod-logs S=api
	$(PROD_COMPOSE) logs -f --tail=200 $(S)

prod-check: ## 生产网络/证书/探针巡检
	deploy/scripts/net-check.sh $${API_HOST:?先设置 API_HOST}

prod-backup: ## 备份数据库
	deploy/scripts/backup-db.sh

prod-migrate-status: ## 生产迁移状态
	$(PROD_COMPOSE) run --rm migrate status
```

---

## 5. 相关文档

- [生产部署与网络加固](../docs/development/deployment.md)：拓扑、HTTPS/Cookie、TURN 验证、多网络矩阵、容量估算、故障排查
- [本地开发环境](setup.md)：dev 容器、常用命令、macOS 排障
- [实时链路](../docs/architecture/realtime-flow.md)、[媒体面](../docs/media/livekit-architecture.md)
- [认证与 RBAC](../docs/auth/authentication.md)、[安全相关数据库设计](../docs/database/schema.md)
