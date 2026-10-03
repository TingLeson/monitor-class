# ClassWatch

**基于浏览器的远程课堂监督系统**（Browser-based Classroom Supervision Platform）。

学生不需要安装任何客户端：打开浏览器 → 输入账号 → 共享**整块显示器** → 老师实时看到每个学生的桌面，
并与指定学生进行私密语音沟通。适用于 OJ 算法训练监督、远程编程课堂、自习与课堂练习监督。

```text
Windows / macOS
      │  Chrome / Edge
      ▼
ClassWatch Student Web
      │  Entire Screen（强制） · 摄像头（可选） · 麦克风（可选）
      ▼
   WebRTC ──► LiveKit SFU ──► Teacher Web Console
```

## 三个核心价值（V1 只做这三件事）

1. **老师控制谁能进入哪个课堂** —— 学生在服务端被授权，前端拿不到未授权的课堂。
2. **学生必须持续共享完整显示器** —— `displaySurface === 'monitor'` 是硬 Gate，选错窗口/标签页直接拒绝进入。
3. **老师能同时监督多个学生，并与指定学生私密沟通** —— 监督墙 + Focus View + 单目标语音。

明确**不做**：学生注册、学生密码、聊天、文件传输、白板、远程控制、录屏回放、
进程扫描、AI 行为检测（完整清单见任务书 §53/§54）。

## 快速开始

```bash
make init         # 生成 .env 并安装依赖
make dev          # 启动 postgres/redis/livekit/migrate/api 容器 + 三个前端
```

`make up` / `make dev` 会先跑一次一次性 `migrate` 服务（应用 versioned SQL 迁移，幂等），
迁移成功后 `api` 才启动；迁移是显式的发布动作，不藏在服务启动流程里。

| 入口         | 地址                                |
| ------------ | ----------------------------------- |
| 学生端       | http://localhost:5173/student/login |
| 老师端       | http://localhost:5174/teacher/login |
| 管理端       | http://localhost:5175/admin/login   |
| API 就绪探针 | http://localhost:8090/readyz        |

```bash
make help         # 查看全部命令
make doctor       # 检查本地环境健康状态
make test lint    # 测试与静态检查
make down         # 停止容器
```

详细步骤、环境变量与故障排查见 [docs/development/setup.md](docs/development/setup.md)。

## 仓库结构

```text
├── apps/
│   ├── student-web/          学生端 SPA（Vue 3 + Vite，:5173）
│   ├── teacher-web/          老师端 SPA（:5174）
│   └── admin-web/            管理端 SPA（:5175）
├── packages/
│   ├── shared-types/         领域类型与错误码（前后端契约）
│   ├── api-client/           统一 HTTP 客户端（Cookie 会话、统一错误结构）
│   └── ui/                   Tailwind v4 主题 + 无业务逻辑基础组件
├── services/api/             Go 后端（Gin + pgx + Redis + LiveKit Server SDK）
│   ├── cmd/                  api / migrate 两个二进制
│   ├── internal/             config · apperr · httpapi · infrastructure · media
│   └── migrations/           versioned SQL migration
├── deploy/                   livekit 配置、反向代理与镜像（Phase 11）
├── docs/                     架构 / 数据库 / 开发流程等文档
├── docker-compose.yml        本地基础设施
├── Makefile                  开发者统一入口
└── .github/workflows/ci.yml  CI
```

## 架构要点（速览）

- **控制面与媒体面严格分离**：PostgreSQL + Go API 是唯一 Source of Truth；
  LiveKit 只搬运媒体。**「LiveKit Room 存在」绝不等于「课堂已开启」**。
- **Classroom ≠ ClassroomRun**：课程容器长期存在，每次「开启课堂」产生一条新的 Run，
  历史、会话与事件都挂在 Run 上。
- **三个 Web 入口物理分离**，共享 `packages/*`，而不是一个页面按角色切换 UI。
- **`ONLINE` 由服务端判定**：必须由 LiveKit Webhook 确认学生发布了 screen share track，
  前端说「我成功了」不算数。
- **媒体层 identity 一律 opaque UUID**，禁止出现姓名、账号、手机号。

完整说明与 Mermaid 架构图见 [docs/architecture/overview.md](docs/architecture/overview.md)。

## 安全模型（诚实说明）

学生账号**没有密码**，仅凭 account 登录。因此知道某个学生账号的人理论上可以冒充该学生——
这是 V1 **主动接受**的业务安全模型，不是缺陷。

但以下能力是工程底线，必须始终成立：服务端 RBAC 与 Ownership 校验、停用账号立即失效、
opaque session token（服务端只存 hash）、Rate Limit、CORS allowlist、CSRF 防护、
LiveKit secret 只存在于后端、短时 LiveKit Token、Webhook 签名校验。

同时必须清楚：`displaySurface === 'monitor'` 是**浏览器端检查**，服务端无法获得
「这条轨道一定来自整块物理显示器」的密码学证明。V1 防御的是误选窗口、故意只共享单个窗口、
中途停止共享等普通课堂逃避行为，**不是**高对抗型考试防作弊系统。

## 开发状态

| Phase | 内容                                                 | 状态    |
| ----- | ---------------------------------------------------- | ------- |
| 0     | 工程骨架（monorepo、容器、迁移、CI、文档）           | ✅ 完成 |
| 1     | 认证与会话（Admin/Teacher 密码、Student 账号、RBAC） | ✅ 完成 |
| 2     | 管理员用户管理（账号列表/创建/启停用/重置密码）      | ✅ 完成 |
| 3     | Classroom 领域与 ClassroomRun（开关课堂、学生名单）  | ✅ 完成 |
| 4     | 学生课堂门户（我的课堂 + PreJoin）                   | ✅ 完成 |
| 5     | 整屏共享 Gate（能力自检 + displaySurface 硬闸门）    | ✅ 完成 |
| 6     | LiveKit：1 老师 + 1 学生（真实媒体链路）             | ✅ 完成 |
| 7     | 多学生监督墙 + Focus View（完整名单 + 动态订阅）     | ✅ 完成 |
| 8     | 运行时状态与事件（Webhook + session_events + WS）    | ✅ 完成 |
| 9     | 摄像头                                               | ⏳      |
| 10    | 麦克风与私密语音                                     | ⏳      |
| 11    | 生产加固（HTTPS/TURN/限流/指标）                     | ⏳      |
| 12    | 性能压测（1 老师 + 20/30 学生）                      | ⏳      |

## 文档

- [架构总览](docs/architecture/overview.md)
- [本地环境搭建](docs/development/setup.md)
- [开发工作流与完成标准](docs/development/workflow.md)
- [数据库设计](docs/database/schema.md)
- [文档地图](docs/README.md)

## 浏览器支持

最新版 **Chrome / Edge**（Windows、macOS 桌面端）。
Safari、Firefox 与移动端浏览器不在 V1 支持范围内——它们无法可靠判断
「是否共享了整块物理显示器」，而这是本系统的核心业务前提。
