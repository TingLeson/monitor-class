# 老师端（teacher-web）

> 本文档覆盖 **Phase 3（课堂管理）** 的内容；监督墙与 Focus View 属于 Phase 7，
> 私密语音属于 Phase 10，会在对应 Phase 追加章节。

老师端的职责是**管理自己的课堂并（后续）监督自己的学生**。它与其他两个入口完全隔离：

| 能力 | 学生端 | 老师端 | 管理端 |
| --- | --- | --- | --- |
| 创建/编辑课堂 | ✗ | ✓（仅自己拥有的） | ✗ |
| 开关课堂 | ✗ | ✓（仅自己拥有的） | ✗（即使 ADMIN 也不能，§4） |
| 维护课堂学生名单 | ✗ | ✓（仅自己拥有的） | ✗ |
| 创建/停用账号 | ✗ | ✗ | ✓ |

---

## 1. 页面与路由

| 路由 | 页面 | 职责 |
| --- | --- | --- |
| `/teacher/login` | 登录 | 账号 + 密码（[authentication.md](../auth/authentication.md)） |
| `/teacher/dashboard` | 概览 | 我的课堂数量 / 开启中数量 + 常用入口 |
| `/teacher/classrooms` | 课堂列表 | 查看状态、开关课堂、进入详情、新建课堂 |
| `/teacher/classrooms/new` | 新建课堂 | 名称 + 描述 |
| `/teacher/classrooms/:id` | 课堂详情 | 编辑信息、维护学生名单、开关课堂 |
| `/teacher/classrooms/:id/monitor` | 监督墙 | **Phase 7**（当前为占位页） |

---

## 2. 课堂列表

```http
GET /api/v1/teacher/classrooms
→ { "classrooms": [ Classroom, ... ] }
```

- **只返回当前老师拥有的课堂**（`owner_teacher_id = 我`）。这不是前端筛选的结果——
  服务端压根不会把别人的课堂下发给这个老师（§14 的同一条原则：授权必须发生在 Backend）。
- **不分页**：一个老师手上的课堂是个位数，分页只会增加 UI 复杂度与状态；
  如果将来出现"一个老师上百个课堂"的真实需求，再加分页才不会伤到任何人。
- 排序稳定（`created_at DESC, id DESC`），保证列表不会在两次刷新之间跳动。

每张卡片/行显示：

| 字段 | 说明 |
| --- | --- |
| 名称 / 描述 | 描述超长截断显示 |
| 状态 | `OPEN` 绿 / `CLOSED` 灰（§7：V1 **只有**这两个状态） |
| 学生数 | `studentCount`，即"哪些学生有权进入这个课堂" |
| 本次开始时间 | `currentRun.openedAt`（仅 OPEN 时有值）——老师最常问的就是"这节课开了多久了" |

---

## 3. 创建与编辑课堂

```http
POST  /api/v1/teacher/classrooms   { "name": "...", "description": "..." }
PATCH /api/v1/teacher/classrooms/:id  { "name"?: "...", "description"?: "..." }
```

- 新课堂一律是 **`CLOSED`**：创建 ≠ 开课。老师点"开启课堂"才会产生一次
  `ClassroomRun`（§6/§8），这样"课堂存在"与"课堂正在进行"是两件事。
- `name` 去空白后 1–80 字符；`description` 去空白后 ≤ 500 字符（空串视为未填写）。
- `PATCH` **只接受 `name` / `description`**：状态只能通过开关课堂接口改变，
  未知字段会被后端拒绝（400）而不是静默忽略——静默忽略会让界面显示"保存成功"却没有变化。
- 允许课堂处于 `OPEN` 时改名：改名不影响任何正在进行的事情，把它变成错误只会
  让老师在课堂上手忙脚乱。

---

## 4. 学生名单

```http
GET    /api/v1/teacher/classrooms/:id/students
POST   /api/v1/teacher/classrooms/:id/students   { "accounts": ["S10086", "S10087"] }
DELETE /api/v1/teacher/classrooms/:id/students/:studentId
```

### 4.1 按账号添加，而不是"从学生目录里勾选"

老师手上的凭据就是**账号**（学生也只用账号登录），所以界面就是一个可以粘贴账号的输入框：
支持换行、逗号、空格分隔。这样做的另一个原因是**隐私与范围**：
提供一个"搜索全部学生"的目录接口，等于让每个老师都能检索全校学生名单，
而 V1 并不需要它（任务书里也没有这个接口）。老师需要谁，就把谁的账号加进来。

后端只接受 `role == STUDENT && status == ACTIVE` 的账号（§11）：
老师账号、管理员账号、已停用账号都不会被加进名单。

### 4.2 批量添加是"部分成功"

```
200 { "students": [操作后的完整名单],
      "rejected": [ { "account": "S10087", "code": "STUDENT_NOT_FOUND" }, ... ] }
```

真实场景里老师一次粘贴十个账号，其中一两个打错字是常态。若整批失败，老师只能反复试错；
若静默跳过，老师会以为全都加上了。因此：

- 合法的账号照常加入；
- 不合法的逐条返回 `code`，界面把它们列出来（账号 + 中文原因），老师改一个再提交一次；
- 已经在本课堂的账号视为成功（幂等），不报错——老师重复提交同一批账号是常见操作。

| rejected code | 含义 | 老师该做什么 |
| --- | --- | --- |
| `STUDENT_NOT_FOUND` | 账号不存在 | 核对账号拼写 |
| `NOT_A_STUDENT` | 该账号是老师/管理员 | 确认是否拿错了账号 |
| `ACCOUNT_DISABLED` | 账号已被停用 | 联系管理员启用，或换人 |
| `INVALID_REQUEST` | 账号格式非法（空、过长、含非法字符） | 修正格式 |

> 课堂 **OPEN 时也允许添加学生**：临时插班是真实需求，加进来的学生会立刻受到同样的监督。

### 4.3 移除

`DELETE` 成功返回 204；如果该学生本来就不在名单里，返回 404 `STUDENT_NOT_ASSIGNED`
（而不是 204）——"我以为移除了，其实从来没加进来"是需要被看见的状态差异。

移除只影响"能不能进入课堂"，**不会**删除账号，也不会影响历史记录（§9 的数据保留立场）。

---

## 5. 开启 / 关闭课堂

```http
POST /api/v1/teacher/classrooms/:id/open    → { "classroom": ..., "run": ... }
POST /api/v1/teacher/classrooms/:id/close   → { "classroom": ..., "run": ... }
```

这是全项目最重要的状态迁移，界面上的每个细节都有原因：

- **二次确认**：开启课堂意味着"被授权的学生现在可以进入，并且必须共享整块屏幕"，
  这不是一个可以随手点的按钮。
- **按钮在请求进行中禁用**：老师双击"开启"是最容易发生的并发来源；服务端有行锁 + 部分唯一索引
  双保险，前端也不该制造无意义的第二次请求。
- **冲突要讲清楚**：如果另一个标签页已经开过课堂，本次请求会得到 `409 CLASSROOM_ALREADY_OPEN`。
  界面不能只弹"操作失败"，而应显示"课堂已经是开启状态"并刷新列表——让老师看到真实状态，
  而不是怀疑自己点错了。
- **每次开启都是一个新 Run**：关闭后再开启，`currentRun.id` 会变化，界面显示的"本次开始于"
  随之刷新。历史 Run 不会被复用也不会被删除（§8）。
- **关闭课堂的完整效果**（学生断开 WebRTC、停止屏幕共享、回课堂页、提示"老师已关闭本课堂"）
  要等 **Phase 6/8** 接入媒体与事件之后才会全部生效；Phase 3 只保证**控制面状态正确**：
  `status` 回到 `CLOSED`、Run 被关闭、`current_run_id` 置空。

---

## 6. 权限（老师端视角）

- 所有 `/api/v1/teacher/**` 端点都同时经过：**会话中间件 → 角色校验（TEACHER）→ CSRF 校验（写方法）**，
  再叠加**每端点独立的 Ownership 校验**（§37/§63）。
- 别的老师的课堂 → `403 CLASSROOM_NOT_OWNER`（不是 404）：任务书 §58 专门为这件事定义了错误码，
  说明产品上要把它和"课堂不存在"区分开。
- **管理员也拿不到这些能力**：ADMIN 会话访问老师端路由会得到 `403 ROLE_FORBIDDEN`（§4）。
  管理员能管的是账号，不是别人的课堂。
- 前端的路由守卫、按钮禁用、确认弹窗**全都是 UX**；用 curl 直接调用同样会被服务端拒绝。

错误码 → 界面行为：

| 错误码 | 界面行为 |
| --- | --- |
| `CLASSROOM_NOT_FOUND` | 提示课堂不存在 + 回列表入口 |
| `CLASSROOM_NOT_OWNER` | 提示这不是你的课堂（不提供"申请权限"这类 V1 不存在的功能） |
| `CLASSROOM_ALREADY_OPEN` / `CLASSROOM_ALREADY_CLOSED` | 提示真实状态并刷新列表 |
| `STUDENT_NOT_ASSIGNED` | 提示该学生不在名单里并刷新名单 |
| `STUDENT_NOT_FOUND` / `NOT_A_STUDENT` / `ACCOUNT_DISABLED` | 出现在批量添加的 rejected 列表里，逐条说明 |
| `INVALID_REQUEST` | 表单字段级提示（名称/描述/账号格式） |
| `ROLE_FORBIDDEN` / `AUTH_REQUIRED` | 由会话守卫处理（重新登录） |
| 未知错误 | 通用提示，绝不显示原始 500 报文 |

---

## 7. 监督墙（Phase 6：能真的看到画面）

进入 `/teacher/classrooms/:id/monitor` 后：

```text
POST /teacher/classrooms/:id/media-token   ← 只允许 owner，课堂必须 OPEN
   ↓  连接 SFU（autoSubscribe = false，§52）
GET  /teacher/classrooms/:id/monitor       ← 业务状态（来自 PostgreSQL + 服务端对媒体面的观测）
   ↓  对 screen.active = true 的学生，手动订阅其屏幕轨道并渲染到卡片主体（§29）
```

- **业务状态与媒体状态分开**（§51）：卡片上的徽章（🟢 正常 / 🔴 屏幕中断 / ⚪ 未连接）来自 Monitor DTO；
  画面来自 LiveKit。绝不拿 LiveKit participant 当业务模型。
- **手动订阅**：`autoSubscribe = false`，只订阅需要看的画面；同一个 participant 不会重复订阅。
- 老师端**可以**显示学生的屏幕画面（这正是监督墙的意义）；Phase 7 会加网格、Focus View 与按可见性动态订阅。
- Phase 6 是"1 老师 + 1 学生"的形态；多学生网格与降质策略属 Phase 7。

## 8. 未做（按 Phase 归属）

| 项 | Phase |
| --- | --- |
| 监督墙（学生卡片、屏幕画面、状态徽章） | 7 |
| Focus View、按可见性动态订阅 | 7 |
| 课堂历史 Run 列表 / 出勤统计 | V1 未定义（数据已在库里，界面属于 V2） |
| 私密语音 | 10 |
| 学生目录搜索、批量导入 CSV | 明确不做（见 §4.1） |
