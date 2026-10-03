# 开发工作流与完成标准

> 本文档固化两件事：**每个 Phase 怎么做**（任务书 §79）与**什么才算做完**（任务书 §83）。
> 目的不是流程表演，而是避免出现「代码编译通过就宣布完成」这种最常见的失败模式。

---

## 1. Phase 路线图

| Phase | 分支 | 目标 | 状态 |
| --- | --- | --- | --- |
| 0 | `phase/00-bootstrap` | 工程骨架：monorepo、容器、迁移机制、CI、文档 | ✅ 已完成 |
| 1 | `phase/01-auth` | 三种登录、Session、RBAC、停用账号 | ✅ 已完成 |
| 2 | `phase/02-admin-users` | 管理员用户管理（创建老师/学生、启停用、重置密码） | ✅ 已完成 |
| 3 | `phase/03-classrooms` | Classroom 领域、授权学生、OPEN/CLOSED、ClassroomRun | ✅ 已完成 |
| 4 | `phase/04-student-portal` | 学生课堂门户（进入按钮 → PreJoin） | ✅ 已完成 |
| 5 | `phase/05-screen-gate` | 整屏共享 Gate（**不接 LiveKit**） | ✅ 已完成 |
| 6 | `phase/06-livekit-screen` | 1 老师 + 1 学生 + 1 屏幕轨道 | ✅ 已完成 |
| 7 | `phase/07-multi-student` | 多学生监督墙、Focus View、手动订阅 | ✅ 已完成 |
| 8 | `phase/08-runtime-state` | WebSocket、LiveKit Webhook、StudentSession、SessionEvent | ✅ 已完成 |
| 9 | `phase/09-camera` | 可选摄像头 + 老师端画中画 | ✅ 已完成 |
| 10 | `phase/10-private-audio` | 学生可选麦克风 + 老师私密语音状态机 | ✅ 已完成 |
| 11 | `phase/11-production` | HTTPS、TURN、反代、限流、CSRF、CORS、指标、重连 | ✅ 已完成 |
| 12 | `phase/12-performance` | 1 老师 + 20/30 学生压测（禁止用假 video 标签冒充） | ⏳ |

**每个 Phase 完成后停止，等待人工确认再进入下一个 Phase。**

---

## 2. 单个 Phase 的固定动作

```text
1. 阅读当前项目                （先看代码现状，不凭记忆）
2. 阅读当前 Phase 文档          （任务书对应章节 + docs/ 相关文档）
3. 检查上一 Phase 实现          （确认前置条件真的成立）
4. 写设计说明                  （数据流、状态迁移、边界、取舍）
5. 实现
6. 写测试                      （单测 + 集成测试；前端补组件/路由测试）
7. 执行测试                    （贴出真实输出，不写“应该能过”）
8. 修 Bug                      （修到真实通过为止）
9. 更新 docs                   （含 Mermaid 图；文档服务于学习，不是 API 罗列）
10. 输出改动总结                （做了什么、没做什么、风险点）
11. Git commit                 （一个 Phase 一个分支，提交信息说明业务意图）
```

明确禁止：

```text
收到整份文档 → 一次性写 100 个文件 → 宣布完成
```

必须 Layer by Layer：先让领域正确，再让媒体跑通，最后才做性能与加固。

---

## 3. 分支与提交约定

- 分支：`phase/NN-topic`（例如 `phase/01-auth`）。
- 提交信息：`type: 中文说明`，type 使用 `feat` / `fix` / `chore` / `docs` / `test` / `refactor`。
  说明写**业务意图**而不是文件变动，例如：
  - ✅ `feat: 实现老师账号密码登录与服务端会话`
  - ❌ `update code`
- 每个 Phase 结束时工作区必须干净（`git status` 无未提交改动）。

---

## 4. Definition of Done（每个 Phase 都必须全部满足）

| 项 | 要求 |
| --- | --- |
| Build PASS | 后端 `go build ./...`；前端 `pnpm build`（三个 app） |
| Unit Test PASS | `make test-api` / `make test-web` |
| Integration Test PASS | 涉及数据库的改动必须连**真实 PostgreSQL** 验证 |
| Lint PASS | `make lint`（gofmt + go vet + ESLint + Prettier），0 error 0 warning |
| Manual Test PASS | 涉及媒体/浏览器的改动必须真开浏览器验证，禁止只用 mock |
| Docs updated | 相关 `docs/**` 与 Mermaid 图同步更新 |
| No TODO hiding 未完成核心逻辑 | 不允许用 TODO 假装完成；未实现的部分要在文档中写明属于哪个 Phase |
| No hardcoded secrets | 密钥只来自环境变量；仓库中不得出现真实密钥 |
| No obvious authorization bypass | 每个受保护端点都要有服务端 RBAC + Ownership 校验 |
| Git clean | 工作区干净，提交信息符合约定 |

### 4.1 授权自检清单（每个涉及资源的端点都过一遍）

```text
1. 会话有效吗？（Session Middleware）
2. 账号是 ACTIVE 吗？（DISABLED 必须立刻失效）
3. 角色允许调用这个路由吗？（RBAC）
4. 这个资源属于当前用户吗？（Ownership）
5. 业务状态允许这个操作吗？（例如 Classroom 是否已 OPEN/CLOSED）
6. 前端隐藏按钮的行为是否被误当成授权？（前端只是 UX，服务端才是边界）
```

---

## 5. 代码规范

- **注释解释 WHY，不解释 WHAT**（任务书 §80）：

  ```go
  // ❌ 废话注释
  // Get classroom
  c := getClassroom(id)

  // ✅ 解释约束与意图
  // Classroom 的 OPEN/CLOSED 只能由创建它的老师修改：即使 ADMIN 也不能通过
  // 普通业务 API 开关别人的课堂（任务书 §4）。这里必须在服务端校验 ownership，
  // 前端隐藏按钮只是 UX。
  if classroom.OwnerTeacherID != currentUser.ID {
      return apperr.New(apperr.CodeClassroomNotOwner)
  }
  ```

- 状态迁移必须写清「为什么允许这条边、为什么不允许那条边」。
- 禁止在日志中写入 password、LiveKit API secret、完整 token、session cookie（任务书 §59）。
- 媒体层 identity 一律使用 opaque UUID，禁止姓名/账号/手机号（任务书 §8/§26/§44）。
- 不新增任务书未定义的功能（任务书 §54）；怀疑需要新增时先停下来确认。

---

## 6. 文档要求

- 每个重要模块都要有对应文档，并且**解释**而不是罗列：

  ```text
  为什么这样设计
  替代方案是什么
  为什么不用替代方案
  数据如何流动
  状态如何变化
  遇到了什么限制
  ```

- 架构图必须维护，优先 Mermaid（任务书 §82）。

  必须长期存在的图：

  ```text
  System Architecture
  Authentication Flow
  Student Join Flow
  Classroom State Machine
  Student Session State Machine
  WebRTC Media Flow
  Teacher Private Audio Flow
  ```

  当前已有的图见 [docs/architecture/overview.md](architecture/overview.md) 与
  [docs/database/schema.md](database/schema.md)。其余图在对应 Phase 补齐。

---

## 7. 代码评审时最常被问的问题

1. 这个状态变化是**谁**触发的？前端说的还是服务端观测到的？
2. 如果两个请求并发，会不会出现两条活跃会话 / 两个 Run？
3. 这个端点被别的角色直接调用（curl）会发生什么？
4. 这个改动会不会让学生在课堂上**突然断掉**？（课堂正在进行时不做破坏性变更）
5. 网络抖动、屏幕共享中断、Chrome 被最小化时，用户看到什么？
