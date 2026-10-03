# ============================================================================
# ClassWatch 三个 SPA 的生产构建镜像（任务书 §5 / §61 / §62）
#
# 产物不是"一个 Node 服务"，而是一堆静态文件（Vite build 输出 apps/*/dist）。
# 之所以做成"镜像 + 一次性发布容器"（compose 里的 web-dist），而不是让运维在
# 服务器上 pnpm build，或者把宿主机的 dist 目录 bind mount 进 Caddy：
#
#   1) bind mount 一个**不存在**的宿主目录时，Docker 会静默创建空目录，
#      站点点开是白屏而**不是**报错 —— "部署成功但站点是空的"是最难查的失败形态。
#      本镜像的 publish 脚本会显式检查 index.html 是否存在，缺了就退出码非 0，
#      caddy 因此不会启动（而不是对外提供一个空站点）。
#   2) 构建环境（Node/pnpm 版本、lockfile）与 CI 完全一致，产物可复现。
#   3) 回滚 = 换回上一个镜像 tag，与 api 的回滚方式一致。
#
# 构建上下文是**仓库根**（需要 pnpm-lock.yaml 与 workspace 定义）：
#   docker build -f deploy/docker/web.Dockerfile -t classwatch-web:dev .
# compose 里已写好 context: .. 与 dockerfile: deploy/docker/web.Dockerfile。
# ============================================================================

ARG NODE_IMAGE=node:26-alpine

# ---------------------------------------------------------------------------
# 构建阶段
# ---------------------------------------------------------------------------
FROM ${NODE_IMAGE} AS build

# 与 package.json 的 packageManager 字段保持一致：pnpm 版本漂移会让 lockfile
# 的解析结果发生变化（表现为"本地能构建、CI 不能"）。
ARG PNPM_VERSION=11.22.0

# ⚠️ 用 npm 安装 pnpm，**不要**用 corepack。
#
# WHY（实测踩到过）：corepack **从 Node 25 起不再随 Node 分发**，
# 所以在 `node:26-alpine` 里 `corepack enable` 会直接以 exit code 127
# （command not found）失败，而报错只有一行 `did not complete successfully`，
# 很容易被误读成网络/权限问题。
# `npm install --global pnpm@<精确版本>` 与 corepack 的效果等价——都把 pnpm
# 钉在 package.json 的 packageManager 版本上——但它在所有 Node 镜像里都可用。
# 版本必须精确（不用 `pnpm@latest`）：pnpm 大版本会改变 lockfile 的解析与
# 依赖年龄策略，漂移的表现是"本地能构建、CI 不能"。
RUN npm install --global "pnpm@${PNPM_VERSION}" \
 && pnpm --version

WORKDIR /src

# 依赖先装：这一层只随 lockfile 变化而变化，改业务代码不会重新下载整个依赖图。
# .npmrc 必须一起复制：里面的 save-exact / strict-peer-dependencies 会影响解析结果。
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./

# 三个 app 与共享包的 package.json 需要先就位，pnpm 才能解析 workspace:* 协议。
COPY apps/student-web/package.json ./apps/student-web/
COPY apps/teacher-web/package.json ./apps/teacher-web/
COPY apps/admin-web/package.json ./apps/admin-web/
COPY packages/ui/package.json ./packages/ui/
COPY packages/api-client/package.json ./packages/api-client/
COPY packages/shared-types/package.json ./packages/shared-types/

# --frozen-lockfile：lockfile 与 package.json 不一致时直接失败。
# WHY 这是硬要求：生产镜像绝不允许"安装时顺手升了一个小版本"。
RUN pnpm install --frozen-lockfile

# 源码。tsconfig.base.json 在仓库根，三个 app 都继承它。
COPY tsconfig.base.json ./
COPY packages ./packages
COPY apps ./apps

# 三个 SPA 的构建（vue-tsc + vite build）。构建期就会暴露类型错误，
# 这是"构建产物能跑"的第一道门。
RUN pnpm build

# 构建产物自检：三个 dist 目录都必须有 index.html。
# 放在构建阶段而不是发布阶段，是为了让失败发生在**构建时**（CI 里可见），
# 而不是部署时才翻车。
RUN for site in student-web teacher-web admin-web; do \
      test -f "/src/apps/$site/dist/index.html" \
        || { echo "FATAL apps/$site/dist/index.html 不存在：Vite 构建没有产出预期产物" >&2; exit 1; }; \
    done \
 && du -sh /src/apps/*/dist

# ---------------------------------------------------------------------------
# 发布阶段
# ---------------------------------------------------------------------------
# 只保留静态文件与一个拷贝脚本：镜像小、攻击面小，且没有 Node 运行时。
FROM alpine:3.22

# 三个站点的产物按站点分目录，与 Caddyfile 里
#   root * {$WEB_ROOT}/student | /teacher | /admin
# 一一对应。目录名故意用角色名（student/teacher/admin）而不是 app 名
# （student-web），这样 Caddyfile 与运维心智一致。
COPY --from=build /src/apps/student-web/dist /srv/student
COPY --from=build /src/apps/teacher-web/dist /srv/teacher
COPY --from=build /src/apps/admin-web/dist /srv/admin

COPY deploy/docker/publish-web.sh /usr/local/bin/publish-web.sh
RUN chmod 0755 /usr/local/bin/publish-web.sh

# 默认目标：compose 会把 webdist 卷挂到这里。
ENV WEB_DEST=/dest

ENTRYPOINT ["/usr/local/bin/publish-web.sh"]
