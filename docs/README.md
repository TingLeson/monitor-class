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
| [frontend/teacher.md](frontend/teacher.md) | 老师端页面、课堂列表与状态、创建/编辑、学生名单（按账号添加与部分成功）、开关课堂的交互约束 | Phase 3 起（Phase 7 扩展） |
| [frontend/student.md](frontend/student.md) | 学生端页面、我的课堂卡片、PreJoin 与隐私告知（§57 原文）、学生间隔离在各层的落点、错误码行为 | Phase 4 起（Phase 5/6 扩展） |
| [architecture/realtime-flow.md](architecture/realtime-flow.md) | 实时事件数据流：为什么业务消息不走 DataChannel、webhook 与客户端事件的双通道分工、状态迁移的幂等/乱序/终态规则、WS 鉴权与作用域隔离、失败与重连语义、进程内 hub → Redis 的演进接缝 | Phase 8 |
| [architecture/observability.md](architecture/observability.md) | 指标清单（14 个族）、为什么手写 exposition、标签基数红线、日志脱敏、告警 PromQL、可信代理下的真实 IP、优雅关闭时序 | Phase 11 |
| [development/deployment.md](development/deployment.md) | 生产部署：Caddy/HTTPS/Cookie 取舍、TURN 与受限网络验证、多网络测试矩阵、升级与回滚、备份恢复演练、资源估算、故障排查表、单实例限制 | Phase 11 |
| [database/state-machines.md](database/state-machines.md) | Classroom / ClassroomRun / StudentSession 状态机：允许的迁移、触发者、副作用、为什么禁止反向与跳变、并发、幂等与乱序处理 | Phase 3 起（Phase 8 补全） |
| [architecture/control-plane.md](architecture/control-plane.md) | 控制面边界与 Source of Truth、请求链中的授权位置、开课/关课事务与并发分析、错误码契约、Phase 6/8 接缝 | Phase 3 |
| [media/webrtc-basics.md](media/webrtc-basics.md) | Track/Source、为什么课堂是 1 上行 N 下行、`autoSubscribe=false` 的理由、客户端与服务端各自能证明什么 | Phase 6 |
| [media/livekit-architecture.md](media/livekit-architecture.md) | Room/Participant/Track、Token 权限位逐字段依据、identity 为什么必须 opaque、Room 生命周期、Cloud 与本地容器两种模式 | Phase 6 |
| [media/track-permissions.md](media/track-permissions.md) | 谁能发布/订阅什么：Token 权限位、服务端 `UpdateSubscriptions` 撤销、客户端 `autoSubscribe=false` 三层落点，以及「合作型客户端可强制、恶意客户端不可强制」的边界 | Phase 7 |
| [media/private-audio.md](media/private-audio.md) | 私密语音设计：IDLE→TALKING(student)→IDLE 状态机、`UpdateSubscriptions` 如何在 SFU 收口"只有被选中的学生听到老师"、学生→老师单向、双向未开麦时的行为、切换与撤销时机、单实例限制与威胁模型边界 | Phase 10 |
| [media/sfu.md](media/sfu.md) | SFU 与 Mesh/MCU 的取舍、规模假设与带宽量级、为什么不能用假 `<video>` 标签压测 | Phase 6 |

## 计划中的文档（按 Phase 产出）

| 文档 | 内容 | Phase |
| --- | --- | --- |
| `labs/browser-screen-capture.md` | Screen Capture API、MediaStream/Track、`displaySurface`、浏览器隐私边界 | 5 |
| `architecture/media-plane.md` | 媒体面边界的完整论述（当前要点已分布在 overview / control-plane / realtime-flow 中） | 待定 |
| `deployment/local.md` | 本地环境细节（已并入 [development/setup.md](development/setup.md)，不再单列） | —— |

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
