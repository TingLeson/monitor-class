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

| 镜像             | Dockerfile 位置           | 构建命令                                                      |
| ---------------- | ------------------------- | ------------------------------------------------------------- |
| API（Go）        | `services/api/Dockerfile` | `make image` 或 `docker build -t classwatch-api services/api` |
| 三个前端静态站点 | 本目录（Phase 11）        | 前端开发期用宿主的 `pnpm dev`，生产再容器化（任务书 §61）     |

## Phase 11 会在这里补什么

- 三个 SPA 的多阶段构建（`pnpm build` → Nginx/静态托管），
- 与 `deploy/nginx/` 的反代配置配合的生产 compose / 编排文件，
- 镜像tag策略与非 root 运行、只读根文件系统等加固项。
