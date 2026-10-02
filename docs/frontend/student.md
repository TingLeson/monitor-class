# 学生端（student-web）

> 本文档覆盖 **Phase 4（课堂门户 + PreJoin）**。整屏共享 Gate 属于 Phase 5，
> 课堂会话页（状态指示、摄像头、麦克风）属于 Phase 6/9/10，会在对应 Phase 追加章节。

学生端是整个系统里**最不该变化**的一端：它每次改版都意味着一次真实的课堂风险。
因此它的页面数量、字段数量、依赖数量都被刻意压到最小。

---

## 1. 页面与路由

| 路由 | 页面 | 职责 |
| --- | --- | --- |
| `/student/login` | 登录 | **只输入账号**（无密码、无注册、无验证码，§2.2） |
| `/student/classrooms` | 我的课堂 | 被授权课堂的卡片列表：已开启 / 未开启 |
| `/student/classrooms/:id` | PreJoin | 进入课堂前的最后一步：说明要求 + 隐私告知 + 确认 |
| `/student/session/:sessionId` | 课堂会话 | **Phase 6 起实现**（当前为占位页） |

登录机制（为什么学生没有密码、会话如何保存）见 [authentication.md](../auth/authentication.md)。

---

## 2. 我的课堂

```http
GET /api/v1/student/classrooms
→ { "classrooms": [ StudentClassroom, ... ] }
```

### 2.1 只返回"我被授权进入的课堂"

授权判定发生在服务端 SQL 的 JOIN 里：

```sql
SELECT c.*, t.display_name AS teacher_name, r.id, r.opened_at
  FROM classroom_students cs
  JOIN classrooms c ON c.id = cs.classroom_id
  JOIN users      t ON t.id = c.owner_teacher_id
  LEFT JOIN classroom_runs r ON r.id = c.current_run_id
 WHERE cs.student_id = $1
```

任务书 §14 明确禁止的做法是：提供一个"全部课堂"接口，让前端自己筛选。
那样做的问题不是"多一次请求"，而是**未授权的数据已经下发到了学生的浏览器里**——
越权已经发生，前端再怎么不显示都没用。

排序：**已开启的排在前面**，其次按创建时间倒序。
学生打开这个页面的第一诉求是"进课堂"，把已开启的放最上面能省掉一次视线搜索。

### 2.2 卡片上有什么（以及为什么这么少）

```text
┌─────────────────────────────┐     ┌─────────────────────────────┐
│ C++ 算法训练                │     │ 数据结构练习                │
│ 王老师                      │     │ 李老师                      │
│ ● 已开启                    │     │ ○ 未开启                    │
│       [进入课堂]            │     │       暂不可进入            │
└─────────────────────────────┘     └─────────────────────────────┘
```

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| 课堂名 / 说明 | `classrooms` | 老师填写 |
| 老师显示名 | `users.display_name` | 学生需要知道这是谁的课堂 |
| 状态 | `classrooms.status` | 只有 `OPEN` / `CLOSED` 两种（§7） |
| 本次开始时间 | `currentRun.openedAt` | 让"这节课已经开始多久了"可见 |

**没有**"班级人数"、**没有**同学名单、**没有**其他任何学生的信息。
这不是还没做，而是任务书 §26 的硬性要求：学生之间互不感知。
接口层面少一个字段，比事后在 UI 上小心翼翼地藏起来可靠得多——
所以 `StudentClassroom` 这个 DTO 里根本没有 `studentCount` 这个字段。

### 2.3 未开启的课堂照样显示

`CLOSED` 的课堂**不会**从列表里消失，而是显示成"未开启 / 暂不可进入"。
原因（§14）：学生需要知道自己**被安排了**这节课。
如果未开启就看不见，学生会以为"老师没把我加进去"，然后开始找老师——
而实际上只是还没到开课时间。

---

## 3. PreJoin：进入课堂前的最后一步

```http
GET /api/v1/student/classrooms/:id
→ { "classroom": StudentClassroom } | 404 STUDENT_NOT_ASSIGNED
```

点击「进入课堂」**不会**立刻连接媒体服务器（§15 Step 2）。先进入 PreJoin 页面，
把"接下来会发生什么"讲清楚：

```text
┌─────────────────────────────────┐
│       C++ 算法训练              │
│                                 │
│ 本课堂需要共享整个电脑屏幕。    │
│                                 │
│ 必须：                          │
│ 🖥 整个显示器                   │
│                                 │
│ 可选：                          │
│ 📷 摄像头                       │
│ 🎤 麦克风                       │
│                                 │
│ [共享整个屏幕并进入课堂]        │
└─────────────────────────────────┘
```

### 3.1 隐私告知（§57，原文展示）

> 进入课堂后，老师能够看到当前共享显示器上的内容，包括你在其他应用程序和浏览器中打开的内容。
> 请关闭与课堂无关的隐私信息后再继续。

这段文字**必须先于任何屏幕捕获请求**出现，并且是原文，不允许为了"看起来简洁"而删除或改写。
偷偷请求屏幕权限是这类系统最容易失去学生信任的做法。

### 3.2 摄像头与麦克风是可选

界面明确分组"必须 / 可选"：**只有整块显示器是进入课堂的必要条件**，
摄像头与麦克风由学生自己决定（§24/§25），老师无法远程打开。
把它们和"必须"混在一起展示，会让学生以为不授权就进不去。

### 3.3 主按钮：先过闸门，再进课堂

主按钮「共享整个屏幕并进入课堂」触发的是**整屏共享 Gate**（见 §4）：

```text
点击
  ↓
能力自检（§17）
  ↓
getDisplayMedia({ video: { displaySurface: 'monitor' }, ... })   ← 用户在这里选择共享源
  ↓
displaySurface === 'monitor' ？
  ├─ 否 → stop 轨道 + 明确拒绝（可重试）
  └─ 是 → 共享已就绪（发布到课堂属 Phase 6）
```

**不会**在页面挂载时请求屏幕权限：请求必须由用户的一次明确点击触发。
在被拒绝、被取消、或浏览器不支持时，页面就地说明原因并给出重试入口，**绝不跳转、绝不假装成功**。
**绝不**在用户选了窗口/标签页之后"将就"进入课堂（§17：不要悄悄降级成窗口共享）。

### 3.4 课堂未开启 / 不在名单里

| 情况 | 界面 |
| --- | --- |
| `status === 'CLOSED'` | "老师尚未开启本课堂" + 回列表入口（主按钮禁用） |
| `404 STUDENT_NOT_ASSIGNED` | "你不在这个课堂的名单里，请联系老师确认" + 回列表入口 |

`404` 同时覆盖"课堂不存在"和"你未被授权"两种情况——这是**有意为之**：
否则学生可以通过试探 id 判断某个课堂是否存在（§63 的最小信息暴露原则）。

---

## 4. 整屏共享 Gate（Phase 5，核心业务闸门）

> 核心原则（§51）：学生必须持续共享**完整的显示器**，才能处于正常的课堂状态。
> 摄像头与麦克风是可选的，屏幕不是。

### 4.1 能力自检（§17，发生在请求权限之前）

进入课堂前检查三件事：

```text
navigator.mediaDevices
navigator.mediaDevices.getDisplayMedia
MediaStreamTrack.prototype.getSettings
```

任一缺失 → **拒绝进入**（主按钮禁用 + 说明），文案：

> 当前浏览器无法确认你是否共享了完整显示器。请使用系统支持的最新版 Chrome 或 Edge。

为什么不在缺能力时"降级成窗口共享"：那样学生进得来、老师却看不到该看的东西，
系统会安静地失去它唯一的业务价值。宁可明确拒绝。

### 4.2 请求整屏：约束只是"偏好"

```js
navigator.mediaDevices.getDisplayMedia({
  video: { displaySurface: 'monitor' },  // 偏好整屏
  audio: false,
  selfBrowserSurface: 'exclude',          // 不把自己这个标签页列为候选
  surfaceSwitching: 'exclude',            // 共享过程中不允许切换共享面
  systemAudio: 'exclude',
  monitorTypeSurfaces: 'include',         // 保留"整个屏幕"选项
  preferCurrentTab: false,
})
```

必须清醒地认识到：**这些只是偏好**。浏览器最终仍然允许用户选择 `monitor` / `window` / `browser`
（任务书 §16 明确写明这一点）。所以真正的闸门在下一步。

### 4.3 真正的闸门：`displaySurface`

```js
const track = stream.getVideoTracks()[0]
const { displaySurface } = track.getSettings()

if (displaySurface !== 'monitor') {
  track.stop()        // ← 必须：否则浏览器会一直显示"正在共享"
  rejectJoin()
}
```

| `settings.displaySurface` | 结果 | 客户端错误码 | 用户看到的 |
| --- | --- | --- | --- |
| `'monitor'` | **通过** | — | 进入"正在共享整个屏幕" |
| `'window'` | 拒绝 | `SCREEN_NOT_MONITOR` | "你选择的是窗口，请重新选择「整个屏幕」" |
| `'browser'` | 拒绝 | `SCREEN_NOT_MONITOR` | "你选择的是浏览器标签页，请重新选择「整个屏幕」" |
| `undefined` / 缺失 | 拒绝 | `SCREEN_API_UNSUPPORTED` | §16 的"无法确认你是否共享了完整显示器"文案 |
| 其它值 | 拒绝 | `SCREEN_NOT_MONITOR` | 同上（按非整屏处理） |

三条纪律：

1. **任何拒绝路径都必须 stop 掉所有轨道**。否则浏览器的"正在共享"指示不会消失，
   学生会以为系统还在看他的屏幕——这是信任问题，不是代码风格问题。
2. **`undefined` 也拒绝**（§16："严格监控模式同样拒绝"）。缺少这个字段意味着浏览器无法证明
   共享的是整块屏幕，此时"放行"等于把闸门交给客户端。
3. 用户取消或拒绝授权（`NotAllowedError`）→ `SCREEN_PERMISSION_DENIED`；
   `NotReadableError`（系统未授予屏幕录制权限、没有可捕获的屏幕等）同样归入此类，
   文案要提到"系统可能未授予屏幕录制权限"，而不是含糊的"操作失败"。

### 4.4 停止共享与重新共享（§22 的客户端一半）

学生随时可能点浏览器的"停止共享"，或者直接关掉共享。客户端必须监听 `track.onended`：

```text
ONLINE（本地：正在共享）
   │  track ended
   ▼
lost：⚠ 已停止屏幕共享
      当前课堂要求持续共享整个屏幕。
      [重新共享整个屏幕]
```

- `ended` 之后立即丢弃轨道引用，避免继续持有一个已失效的 MediaStream。
- 重新共享会**重新走一遍完整 Gate**（包括 `displaySurface === 'monitor'` 的检查），
  不存在"上次通过过就一直放行"的捷径。
- 服务端的另一半（LiveKit `track_unpublished` webhook、`SessionEvent` 记录、老师端的红色告警）
  属于 Phase 6/8：**不能只相信前端主动报告**（§46）。

### 4.5 这一步证明了什么、没证明什么（§19）

必须说清楚：`displaySurface === 'monitor'` 是**浏览器客户端检查**。
服务端可以验证"学生是否发布了 ScreenShare 轨道"，但无法获得"这条轨道一定来自整块物理显示器"
的密码学证明。

因此本 Gate 防御的是：误选标签页/窗口、故意只共享单个窗口、共享到一半停掉。
它**不是**高对抗考试防作弊系统——主动修改前端代码、自研 WebRTC 客户端的攻击者不在 V1 威胁模型内。

### 4.6 本机真实浏览器验证情况（诚实记录）

在开发机上用 **Chrome 154（macOS）** 通过 CDP 驱动真实浏览器跑了一遍 PreJoin 流程
（不是 jsdom/happy-dom 模拟，是真的 Chromium）：

| 观察项 | 实测结果 |
| --- | --- |
| §17 能力自检 | 通过（`mediaDevices` / `getDisplayMedia` / `getSettings` 均为 function），页面**没有**出现"无法确认你是否共享了完整显示器"的阻断提示 |
| 按钮启用条件 | 课堂 `OPEN` + 能力满足 → 按钮可点击 |
| §57 隐私告知 | 渲染在请求之前 |
| 点击后是否真的调用捕获 | 是（真实 `getDisplayMedia` 被调用并进入捕获阶段） |
| 约束字典 | 被 Chrome 接受，没有 TypeError / 约束拒绝——`displaySurface` / `monitorTypeSurfaces` / `surfaceSwitching` / `selfBrowserSurface` / `systemAudio` / `preferCurrentTab` 对当前 Chrome 都有效 |
| §56 不做自身预览 | 全页 `<video>` 元素数量为 0 |
| 失败路径的措辞 | 本机未授予屏幕录制权限，返回 `NotReadableError`；界面显示「无法开始屏幕捕获」并**指向系统"屏幕录制"权限设置**，且**没有**错误地说成"请重新选择「整个屏幕」"（两者是不同的失败，必须给不同的话） |

**仍然无法自动验证的部分**：`displaySurface` 的真实取值（`monitor` / `window` / `browser`）
需要在*已授予屏幕录制权限*的机器上由人工点选，因为 macOS 的屏幕录制权限（TCC）
不允许自动化进程直接开始捕获。因此这一条以人工清单的形式保留在下面。

人工验证清单（§65 的 Case 6/7/8，任一台普通开发机 5 分钟可完成）：

| # | 操作 | 期望 |
| --- | --- | --- |
| 1 | 在共享选择框里选 **Chrome 标签页** | 立即停止捕获 + 提示"请重新选择「整个屏幕」"，**不进入**课堂 |
| 2 | 选 **某个窗口**（如 VSCode） | 同上 |
| 3 | 选 **整个屏幕** | 进入"正在共享整个屏幕" |
| 4 | 共享中点浏览器的"停止共享" | 出现"已停止屏幕共享 / 重新共享整个屏幕" |
| 5 | 点"重新共享整个屏幕"并仍选窗口 | 再次被拒绝（不存在"上次通过就一直放行"） |

---

## 5. 课堂会话页（Phase 6：真实媒体）

Gate 通过后**才**调用 join（§18 的顺序不可颠倒），然后进入 `/student/session/:sessionId`：

```text
Gate 通过（拿到 monitor 轨道）
   ↓  POST /student/classrooms/:id/join   ← 服务端再校验一遍授权与课堂状态
   ↓  拿到 { sessionId, livekitUrl, token }（token 只放内存，不进 URL / Web Storage）
   ↓  连接 SFU（autoSubscribe = false）
   ↓  发布**同一条**轨道（source = ScreenShare）——不再二次请求屏幕权限（§20）
```

会话页显示（§56）：`🖥 正在共享整个屏幕`、网络质量、当前状态。
**不显示自己的屏幕预览**——避免 screen-in-screen。

| 情况 | 行为 |
| --- | --- |
| 学生点了浏览器的"停止共享" | 进入 `⚠ 已停止屏幕共享`，给「重新共享整个屏幕」；重新点会**重跑完整 Gate**（含 `displaySurface` 检查），成功后直接发布新轨道——**不重新 join**（身份与权限都没变，rejoin 只会让老师端卡片闪断） |
| 老师关闭课堂 | 本 Phase 用低频轮询发现（15s 一次），提示"老师已经关闭本课堂" → 断开媒体 + 停止本地捕获 + 回课堂列表；Phase 8 会用 WebSocket 取代轮询 |
| Token 过期 / 连接失败 | 可读提示 + 重试（重试会重新 join 拿新 Token） |
| 刷新页面 | token 只在内存里，刷新即丢失 → 明确提示并回课堂列表（§44 的必然结果，不是 Bug） |

> 服务端**不接受**学生自报的任何状态：`ONLINE` 由服务端观测 LiveKit 房间得出（见 §6）。

## 6. 学生之间的隔离（贯穿所有 Phase 的约束）

任务书 §26 的要求，在每一层都有对应落点：

| 层 | 做法 |
| --- | --- |
| HTTP DTO | `StudentClassroom` 不含任何其他学生的字段（没有 `studentCount`、没有名单） |
| SQL | 学生接口一律以 `WHERE cs.student_id = $1` 过滤，不存在"先取全部再筛" |
| UI | 不展示人数、不展示同学；没有"谁在线"这类列表 |
| 媒体层（Phase 6+） | 学生客户端 `autoSubscribe = false`，且不会订阅其他学生的任何 Track |
| 身份 | LiveKit identity 使用 opaque UUID，不含姓名/账号（§8/§44） |

> 注意架构边界：在共享同一个 SFU Room 的前提下，"互不感知"是**产品与媒体权限层**的隔离，
> 不等于对自定义客户端的协议级匿名性（§26 末尾的说明）。

---

## 7. 错误码 → 界面行为

| 错误码 | 界面行为 |
| --- | --- |
| `AUTH_REQUIRED` | 由会话守卫处理：回登录页（这是正常路径，不弹错误） |
| `ACCOUNT_DISABLED` | 提示"账号已停用，请联系管理员"并回登录页 |
| `STUDENT_NOT_ASSIGNED` | 提示"你不在这个课堂的名单里" + 回列表 |
| `RATE_LIMITED` | 提示稍后重试 |
| `NETWORK_ERROR` | 提示网络异常并允许重试（列表页保留重试按钮） |
| 未知错误 | 通用提示；**绝不**把 500 的原始报文显示给学生 |

---

## 8. 未做（按 Phase 归属）

| 项 | Phase |
| --- | --- |
| "已停止共享 → 重新共享"的服务端一半（webhook + 事件） | 8 |
| 摄像头开关 + 老师端画中画 | 9 |
| 麦克风开关 + 老师的私密语音请求提示 | 10 |
| 学生端显示自己的屏幕预览 | **明确不做**（§56：避免 screen-in-screen） |
