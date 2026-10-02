# 本地开发环境搭建

> 目标：任意一台开发机，**5 条命令**之内跑起 ClassWatch 的完整本地环境。
>
> ```bash
> git clone <repo> && cd monitor-class
> make init     # 生成 .env + 安装依赖
> make dev      # 启动容器（postgres/redis/livekit/api）+ 三个前端
> ```

---

## 1. 前置要求

| 工具 | 版本 | 检查命令 | 说明 |
| --- | --- | --- | --- |
| Docker Desktop | 29+（含 compose v2） | `docker compose version` | 提供 PostgreSQL / Redis / LiveKit |
| Go | 1.26+ | `go version` | 后端 |
| Node.js | 22+（推荐 26，见 `.nvmrc`） | `node --version` | 三个前端 |
| pnpm | 11+ | `pnpm --version` | monorepo 包管理（`corepack enable` 可自动获取） |
| make | 任意 GNU make | `make --version` | 统一的开发入口 |

macOS 建议给 Docker Desktop 至少 **4GB 内存**（PostgreSQL + Redis + LiveKit + API 四个容器）。

## 2. 快速开始

```bash
make init      # 1) 从 .env.example 生成 .env  2) pnpm install  3) go mod download
make up        # 2) 启动 postgres / redis / livekit / migrate / api（后台，migrate 自动跑完退出）
make doctor    # 3) 检查依赖连通性
make dev-web   # 4) 前台启动三个前端 dev server
```

或者直接 `make dev`：它会先确保容器已启动，然后在前台运行三个前端
（`Ctrl-C` 只退出前端，容器仍在后台，用 `make down` 停止）。

### 2.1 访问地址

| 入口 | 地址 | 说明 |
| --- | --- | --- |
| 学生端 | http://localhost:5173/student/login | Phase 0 为路由骨架页 |
| 老师端 | http://localhost:5174/teacher/login | 同上 |
| 管理端 | http://localhost:5175/admin/login | 同上 |
| API | http://localhost:8090/api/v1/meta | 服务元信息 |
| API 存活探针 | http://localhost:8090/healthz | 只表示进程活着 |
| API 就绪探针 | http://localhost:8090/readyz | 真实检查 PostgreSQL / Redis / LiveKit |
| LiveKit | ws://localhost:7880（信令） · 7881/tcp · 50000-50100/udp | 开发者控制台不启用 |
| PostgreSQL | `localhost:5432`（`classwatch` / 见 `.env`） | 端口由 `POSTGRES_PORT` 控制 |
| Redis | `localhost:6380` | 端口由 `REDIS_PORT` 控制（6379 常被其它项目占用） |

容器拓扑（`docker compose ps -a` 应能看到五项）：

```text
postgres (healthy) ─┐
redis    (healthy) ─┼─► migrate (一次性，Exited 0) ─► api (healthy)
livekit  (healthy) ─┘
```

`migrate` 是一个**一次性服务**：应用 versioned migration 后以退出码 0 结束，
`api` 通过 `service_completed_successfully` 等它完成后才启动。
这样「发布动作」在编排层是可见、可重跑的一步，而不是藏在服务启动流程里的隐藏副作用。

## 3. 环境变量

`.env` 由 `make init` 从 `.env.example` 复制而来，**已被 `.gitignore` 忽略**。
所有变量都带注释说明，关键几项：

| 变量 | 作用 | 注意 |
| --- | --- | --- |
| `APP_ENV` | `development` / `test` / `production` | 影响日志格式（JSON vs text）与 Gin 模式 |
| `API_PORT` | API 在**宿主机**发布的端口（容器内固定 8080） | 默认 `8090`：8080 在开发机上常被其它项目占用 |
| `VITE_API_PROXY_TARGET` | 三个前端 dev server 的代理目标 | 必须与 `API_PORT` 一致，否则前端会静默打到错误的后端 |
| `POSTGRES_PORT` / `REDIS_PORT` | PostgreSQL / Redis 在宿主机的端口 | 默认 `5432` / `6380`（6379 常被其它项目占用） |
| `DATABASE_URL` | 宿主机直连数据库 | 容器内由 compose 覆盖为 `postgres:5432` |
| `REDIS_ADDR` | Redis 地址 | 容器内为 `redis:6379` |
| `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | LiveKit 服务端凭据 | **只注入后端与 LiveKit 容器**；绝不能进前端产物 |
| `LIVEKIT_URL` | 下发给浏览器的信令地址 | 必须是**浏览器可达**的地址（`ws://localhost:7880`） |
| `LIVEKIT_API_URL` | 后端调用 LiveKit HTTP API | 容器内为 `http://livekit:7880` |
| `CORS_ALLOWED_ORIGINS` | 允许携带 Cookie 的来源白名单 | 禁止用 `*`；新增端口必须同步修改 |
| `DB_AUTO_MIGRATE` | 启动时自动迁移 | 默认 `false`；生产必须为 `false` |
| `SESSION_COOKIE_SECURE` | Cookie 是否要求 HTTPS | 本地 http 必须为 `false`，生产必须为 `true` |

> 端口为什么不用「标准默认值」：开发机上 8080 与 6379 经常已被其它项目占用，
> 而端口冲突的表现是容器起不来、或前端静默连到别的后端，排查成本很高。
> 因此 ClassWatch 把**所有宿主机端口都做成 `.env` 变量**，
> 并让前端代理目标读取同一份 `.env`（`apps/*/vite.config.ts` 从仓库根加载），
> 保证「改一处、全链路一致」。

> `.env.example` 里的 key/secret 是**本地开发占位值**，唯一目的是让 compose 能跑起来。
> 任何真实环境都必须由部署平台注入自己的密钥。

## 4. 常用命令

```bash
make help            # 列出全部目标

# 环境
make up / down       # 启动 / 停止容器（down 保留数据卷）
make restart         # 重建并重启
make ps / logs       # 容器状态 / 跟踪日志（make logs S=api）
make doctor          # 健康检查（healthz + readyz + LiveKit + 容器状态）
make nuke            # 停止并删除数据卷（清空本地数据库）

# 开发
make dev             # 容器 + 三个前端（前台）
make dev-web         # 只启动前端
make dev-api         # 在宿主直接跑 Go API（改代码需手动重启）

# 数据库
make migrate-up      # 应用迁移（幂等）
make migrate-status  # 已应用的迁移
make db-shell        # psql
make sql Q="select 1"

# 账号（破窗工具，Phase 1 起）
make create-admin    # 交互式创建初始管理员（密码不回显、不进 shell 历史）
make create-user ROLE=TEACHER   # 创建老师 / 学生（make create-user ROLE=STUDENT）
make reset-password ACCOUNT=teacher001
make list-users      # 列出账号（可加 ROLE=STUDENT 过滤）

# 质量
make lint            # gofmt + go vet + ESLint + Prettier 检查
make fmt             # 自动格式化 Go 与前端
make test            # Go 单测 + 前端单测
make test-integration# 需要真实 PostgreSQL 的集成测试（复用 compose 里的实例）
make ci              # 与 CI 等价的完整检查
```

### 4.1 第一次使用：创建管理员与测试账号

```bash
make up                # 容器起来后 migrate 已自动应用迁移
make create-admin      # 交互式输入 account / display name / password
make create-user ROLE=TEACHER   # 造一个老师账号便于本地联调
make create-user ROLE=STUDENT   # 学生账号不需要密码（业务规则，不是遗漏）
make list-users
```

密码**只能**从标准输入传入：不支持命令行明文密码参数，避免它进入 shell history、`ps` 输出与 CI 日志。
`adminctl` 运行在 `tools` profile 里，因此不会随 `make up` 启动。

登录方式（Phase 1 起）：

| 入口 | 登录凭据 | 相关文档 |
| --- | --- | --- |
| 学生端 `/student/login` | 仅账号（无密码、无注册，任务书 §2.2） | [authentication.md](../auth/authentication.md) |
| 老师端 `/teacher/login` | 账号 + 密码（Argon2id） | 同上 |
| 管理端 `/admin/login` | 账号 + 密码 | 同上 |

当前这台开发机的本地库里已经存在下列**演示账号**（只存在于本地 Docker 数据卷中，
执行 `make nuke` 会一并清空）：

| 账号 | 密码 | 角色 | 显示名 |
| --- | --- | --- | --- |
| `admin` | `classwatch-admin-2026` | ADMIN | 系统管理员 |
| `teacher001` | `classwatch-teacher-2026` | TEACHER | 李老师 |
| `S10086` | 无（学生免密） | STUDENT | 张三 |
| `S10087` | 无 | STUDENT | 李四 |
| `S10088` | 无 | STUDENT | 王五 |

> 这些是**本地开发占位凭据**，仅用于在这台机器上点开三个入口看效果。
> 任何共享或生产环境都必须用 `make create-admin` 重新创建自己的账号与强密码。

单独操作某个前端/包：

```bash
pnpm --filter @classwatch/student-web dev
pnpm --filter @classwatch/api-client test
pnpm -r typecheck
```

## 5. 数据库迁移

- 迁移文件：`services/api/migrations/NNNN_name.sql`，**编号只增不改**。
- 执行器：`services/api/cmd/migrate`（编译进容器镜像，二进制路径 `/app/migrate`）。
- 机制：`pg_advisory_lock` 串行化 → 每个文件一个事务 → 记录到 `schema_migrations` → 可重复执行。

```bash
make migrate-up                    # 应用（内部：docker compose run --rm migrate up）
make migrate-status                # 查看
docker compose run --rm migrate status   # 等价写法
```

`make up` 会自动执行一次 `migrate`（一次性服务），因此日常开发不需要手动调用；
`make migrate-up` 用于改完 SQL 后单独重跑。

为什么不在服务启动时自动迁移（任务书 §60）：
迁移是**发布动作**，需要可评审、可回滚、可观测；把它藏进启动流程会让
「服务起来了但 schema 半新半旧」这种故障极难排查。`DB_AUTO_MIGRATE` 默认关闭。

## 6. 测试

| 层次 | 命令 | 说明 |
| --- | --- | --- |
| 后端单测 | `make test-api` | 不依赖数据库，任何机器都能跑通 |
| 后端集成测试 | `make test-integration` | 需要真实 PostgreSQL：自动建 `classwatch_test` 库并显式传入 `TEST_DATABASE_URL` |
| 前端单测 | `make test-web` | Vitest + happy-dom |
| 手工 WebRTC 测试 | Phase 6 起 | 必须真开 1 个老师浏览器 + 2 个学生浏览器，禁止用 mock 宣布完成 |

集成测试是**显式开启**的：`.env.example` 里 `TEST_DATABASE_URL` 默认被注释掉，
所以 `go test ./...` 在没有数据库的机器上也会全绿（相关用例自动 Skip）。
想直接跑 `go test` 时手动导出即可：

```bash
cd services/api
TEST_DATABASE_URL='postgres://classwatch:classwatch_dev_password@localhost:5432/classwatch_test?sslmode=disable' \
  go test ./internal/infrastructure/... -v
```

## 7. 故障排查

| 现象 | 原因与处理 |
| --- | --- |
| `make up` 报 `POSTGRES_PASSWORD 未设置` | 没有 `.env`。执行 `make init` |
| `readyz` 返回 503，`postgres: error` | 容器还没起来或端口被占用：`make ps`、`make logs S=postgres`；本地已有 PostgreSQL 时改 `.env` 里的 `POSTGRES_PORT` |
| 端口 5173/5174/5175 被占用 | 三个 app 都设置了 `strictPort`，不会静默换端口。先释放端口，避免出现「以为在测学生端其实连到了老师端」 |
| `make up` 报 `port is already allocated` | 宿主机端口被别的项目占用：改 `.env` 里的 `API_PORT` / `POSTGRES_PORT` / `REDIS_PORT` 后 `make up`；改 `API_PORT` 时必须同步改 `VITE_API_PROXY_TARGET` |
| 前端能打开但接口全部失败（连到了别的服务） | 检查 `VITE_API_PROXY_TARGET` 是否指向本项目的 `API_PORT`；改完 `.env` 需要重启前端 dev server |
| 前端请求 404 / CORS 报错 | 前端通过 Vite 代理访问 `/api`；若直连 API，需要把来源加入 `CORS_ALLOWED_ORIGINS` 并重启 api |
| Cookie 登录后立刻失效（Phase 1 起） | 本地是 http，`SESSION_COOKIE_SECURE` 必须为 `false`；跨端口场景下前后端必须是同源（Vite 代理保证） |
| `livekit: error` 但容器在跑 | 检查 `.env` 中 `LIVEKIT_API_KEY/SECRET` 与 `LIVEKIT_KEYS` 注入是否一致；`make logs S=livekit` |
| PostgreSQL 大版本升级后起不来 | 数据卷与 `PGDATA` 布局绑定：`make nuke` 清空本地数据后重建（开发库无重要数据） |
| `pnpm install` 报 peer 冲突 | 先 `pnpm install` 保证 lockfile 一致；不要用 `--force` 掩盖，应修正版本 |
| `docker build` / `make up` 卡住几分钟没输出 | macOS Docker Desktop 的凭据助手 `docker-credential-desktop` 有时会挂住（会一直等待钥匙串）。用隔离的 docker 配置绕过（不改动你的 `~/.docker/config.json`）：见下方「凭据助手卡死」 |
| WebRTC 在某些网络下连不上 | 本地 LiveKit 只监听本机/局域网；跨网络与 TURN 属于 Phase 11，见 `docs/deployment/` |

### 7.1 凭据助手卡死（macOS Docker Desktop）

症状：`docker build` 或 `docker compose build` 长时间停住，`ps aux | grep docker-credential` 能看到挂起的进程。
处理：

```bash
pkill -f docker-credential-desktop

# 用一份不带 credsStore 的隔离配置构建（插件目录要一并复制，否则找不到 compose 插件）
mkdir -p /tmp/docker-noauth
cp -R ~/.docker/cli-plugins /tmp/docker-noauth/cli-plugins
cp -R ~/.docker/contexts /tmp/docker-noauth/contexts 2>/dev/null || true
python3 - <<'PY'
import json, pathlib
cfg = json.load(open(pathlib.Path.home() / '.docker/config.json'))
cfg.pop('credsStore', None)
pathlib.Path('/tmp/docker-noauth/config.json').write_text(json.dumps(cfg, indent=2))
PY

DOCKER_CONFIG=/tmp/docker-noauth DOCKER_HOST="unix://$HOME/.docker/run/docker.sock" \
  docker compose build api
```

镜像构建成功后 `make up` 会直接复用缓存层，不需要每次都这样绕。

## 8. 浏览器要求

V1 只支持 **最新版 Chrome / Edge（Windows、macOS 桌面端）**，且从 Phase 5 起进入课堂前必须通过能力检查：

```text
navigator.mediaDevices
navigator.mediaDevices.getDisplayMedia
MediaStreamTrack.prototype.getSettings
track.getSettings().displaySurface === 'monitor'
```

Safari / Firefox / 移动端浏览器**不在 V1 支持范围**：它们无法可靠地判断
「用户是否共享了整块物理显示器」，而这是本系统的核心业务前提。

> 本地开发注意：`getDisplayMedia` 只在**安全上下文**中可用。
> `http://localhost` 被浏览器视为安全上下文，因此本地无需 HTTPS；
> 但用局域网 IP（如 `http://192.168.x.x:5173`）访问时浏览器会拒绝，
> 需要 HTTPS 或 `chrome://flags/#unsafely-treat-insecure-origin-as-secure`。
