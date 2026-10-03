# deploy/docker

## 为什么 API 的 Dockerfile 不在这个目录

任务书 §36 的目录规划里有 `deploy/docker/`，但**后端镜像的 Dockerfile 刻意放在
`services/api/Dockerfile`**，与 `go.mod` 放在一起。原因：

1. **构建上下文最小化**：`docker build services/api` 只把 Go 模块目录传给 daemon。
   若把 Dockerfile 放在 `deploy/docker/`，构建上下文会变成整个 monorepo
   （含 `node_modules`、`apps/*/dist`），镜像构建会变慢，也更容易把无关文件打进镜像。
2. **就近原则**：改 Go 依赖（`go.mod`/`go.sum`）与改 Dockerfile 通常是一次改动，
   放在同一目录可以一个提交完成，也不会出现「Dockerfile 忘了同步 Go 版本」的漂移。

因此：

| 镜像             | Dockerfile 位置                | 构建命令                                                               |
| ---------------- | ------------------------------ | ---------------------------------------------------------------------- |
| API（Go）        | `services/api/Dockerfile`      | `make image` 或 `docker build -t classwatch-api services/api`          |
| 三个前端静态站点 | `deploy/docker/web.Dockerfile` | `docker build -f deploy/docker/web.Dockerfile -t classwatch-web:<v> .` |

## Phase 11 补上了什么

| 文件             | 作用                                                                                                                                       |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `web.Dockerfile` | 三个 SPA 的多阶段构建（`pnpm install --frozen-lockfile` → `pnpm build`），产物按 `student/teacher/admin` 分目录；运行时镜像里**没有 Node** |
| `publish-web.sh` | 一次性容器把镜像里的产物发布到 `webdist` 卷（Caddy 只读挂载）                                                                              |

### 为什么是"镜像 + 一次性发布容器"，而不是 bind mount 宿主的 `dist/`

1. **bind mount 一个不存在的宿主目录时，Docker 会静默创建一个空目录**，
   站点点开是白屏而**不是**报错——"部署成功但站点是空的"是最难查的失败形态。
   `publish-web.sh` 会检查三个 `index.html`，缺一个就让容器以非 0 退出，
   `caddy` 的 `depends_on: service_completed_successfully` 因此拒绝启动。
2. **构建环境与 CI 完全一致**：Node/pnpm 版本与 `pnpm-lock.yaml` 都锁在镜像里，
   不会出现"服务器上的 Node 比 CI 新一个小版本，产物不同"。
3. **回滚方式与 API 一致**：换回上一个镜像标签（见 `deploy/README.md` 步骤 10）。

### 仍然不做的事（明确记录，避免"以为做了"）

- 不做镜像签名 / `cosign` 校验：当前只依赖"不可变标签 + 私有 registry"。
- 发布容器不用只读根文件系统：它只跑一次 `cp` 就退出，收益极低。
