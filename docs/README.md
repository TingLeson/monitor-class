# ClassWatch 文档地图

> 文档服务学习，不只是 API 罗列。每篇文档都要回答：**为什么这样设计、替代方案是什么、
> 为什么不用替代方案、数据如何流动、状态如何变化、遇到了什么限制**（任务书 §81）。

---

## 已有文档

| 文档 | 内容 | 产出 Phase |
| --- | --- | --- |
| [architecture/overview.md](architecture/overview.md) | 系统架构、控制面/媒体面分离、领域模型、进入课堂主流程、技术选型取舍、安全与威胁模型 | Phase 0 |
| [development/setup.md](development/setup.md) | 本地环境搭建、命令、环境变量、迁移用法、故障排查 | Phase 0 |
| [development/workflow.md](development/workflow.md) | Phase 路线图、每个 Phase 的固定动作、Definition of Done、代码规范 | Phase 0 |
| [database/schema.md](database/schema.md) | 完整目标 schema、约束与不变量、索引与查询、迁移策略 | Phase 0 起（表随 Phase 落地） |
| [auth/authentication.md](auth/authentication.md) | 三种登录方式、Argon2id 密码存储、opaque session 与 Cookie 策略、CSRF、限流、破窗工具 | Phase 1 |
| [auth/rbac.md](auth/rbac.md) | 角色矩阵、请求授权链、入口隔离、401/403 语义、资源级授权规矩、反模式清单 | Phase 1 |
| [frontend/admin.md](frontend/admin.md) | 管理端页面、账号列表与筛选、创建规则（为什么没有管理员选项/学生无密码框）、启停用与重置密码的取舍 | Phase 2 |

## 计划中的文档（按 Phase 产出）

| 文档 | 内容 | Phase |
| --- | --- | --- |
| `database/state-machines.md` | Classroom / StudentSession 状态迁移表：允许的边、触发者、副作用 | 3 |
| `architecture/control-plane.md` | 控制面职责边界、事务与并发（`FOR UPDATE`）、错误码契约 | 3 |
| `frontend/student.md` | 学生端页面、PreJoin、隐私告知、状态指示器（不显示自身预览的原因） | 4 |
| `frontend/teacher.md` | 监督墙、Focus View、手动订阅策略、私密语音交互 | 7 |
| `labs/browser-screen-capture.md` | Screen Capture API、MediaStream/Track、`displaySurface`、浏览器隐私边界 | 5 |
| `media/webrtc-basics.md` | SDP/ICE/Track 基础，为什么课堂场景是 1 上行 N 下行 | 6 |
| `media/livekit-architecture.md` | Room/Participant/Track、Token 与权限位、为什么 `auto_create=false` | 6 |
| `media/sfu.md` | SFU 与 Mesh/MCU 的取舍、订阅控制与带宽 | 6 |
| `architecture/media-plane.md` | 媒体面边界、Webhook 作为唯一权威观测、`ONLINE` 判定权 | 8 |
| `architecture/realtime-flow.md` | WebSocket 业务事件流、与 DataChannel 的分工 | 8 |
| `media/track-permissions.md` | 各角色的 publish/subscribe 权限矩阵、学生间隔离如何实现 | 7-10 |
| `media/private-audio.md` | 老师私密语音状态机、订阅权限切换、学生未授权麦克风时的行为 | 10 |
| `deployment/local.md` | 本地环境细节（与 setup.md 互链） | 0/11 |
| `deployment/production.md` | HTTPS、TURN/TLS、反代、限流、指标、多网络测试 | 11 |

## 目录约定

```text
docs/
├── architecture/   系统结构、平面划分、数据流
├── auth/           认证与授权
├── database/       schema 与状态机
├── media/          WebRTC / LiveKit / 权限 / 私密语音
├── frontend/       三个 Web 入口的页面与交互
├── development/    环境搭建与工作流
├── deployment/     本地与生产部署
└── labs/           学习型讲义（例如浏览器屏幕捕获）
```

## 维护规则

1. 改了行为就改文档，**同一个提交里完成**。
2. 架构图与代码不一致时，视为 Bug：优先怀疑代码。
3. 禁止把「100% 防作弊」「绝对无法绕过」这类表述写进任何文档或产品文案
   （任务书 §19：V1 不是高对抗考试防作弊系统）。
4. 文档里出现的每个状态、错误码、事件类型都必须与代码常量、数据库 `CHECK` 三处一致。
