#!/bin/sh
# ============================================================================
# 把镜像里已经构建好的三个 SPA 产物发布到目标卷（compose 的 webdist）。
#
# 为什么需要这一步：Caddy 是独立容器，它无法直接读取另一个镜像里的文件，
# 只能通过共享卷。所以用一个**一次性容器**（compose 服务 web-dist）在每次发布时
# 把产物拷贝到卷里，caddy 通过 depends_on: service_completed_successfully 等它。
#
# 这个脚本运行在 alpine 的 busybox ash 里（没有 bash），因此：
#   - 用 `set -eu`（ash 支持 pipefail 的版本差异较大，这里刻意不依赖它）；
#   - 只用 POSIX 语法。
#
# WHY 先清空目标目录再拷贝：
#   只做覆盖的话，上一次发布里已经下线的旧 hash 资源会留在卷里。
#   回滚时看起来"成功了"，但卷里是两次发布的混合体 —— 这类问题只会以
#   "某个用户拿到了不该存在的旧 chunk"的形式偶发出现。
#
# WHY 先检查 index.html：
#   产物不完整时**必须**让容器以非 0 退出。caddy 的 depends_on 会因此拒绝启动，
#   运维看到的是"部署失败"，而不是"站点打开是白屏"。
# ============================================================================
set -eu

SRC_ROOT="${SRC_ROOT:-/srv}"
DEST_ROOT="${WEB_DEST:-/dest}"

if [ ! -d "$DEST_ROOT" ]; then
  echo "FATAL 目标目录不存在：${DEST_ROOT}（compose 是否挂载了 webdist 卷？）" >&2
  exit 1
fi

# ── 1. 自检：三个站点都必须有 index.html ───────────────────────────────────
missing=0
for site in student teacher admin; do
  if [ ! -f "$SRC_ROOT/$site/index.html" ]; then
    echo "FATAL 缺少 $SRC_ROOT/$site/index.html：构建产物不完整" >&2
    missing=1
  fi
done
[ "$missing" -eq 0 ] || exit 1

# ── 2. 发布（幂等）────────────────────────────────────────────────────────
for site in student teacher admin; do
  target="$DEST_ROOT/$site"
  rm -rf "$target"
  mkdir -p "$target"
  cp -a "$SRC_ROOT/$site/." "$target/"
done

# ── 3. 发布后再校验一次（防"拷贝到一半磁盘满"）────────────────────────────
for site in student teacher admin; do
  if [ ! -s "$DEST_ROOT/$site/index.html" ]; then
    echo "FATAL 发布后 $DEST_ROOT/$site/index.html 为空：请检查磁盘空间" >&2
    exit 1
  fi
  echo "published $site -> $DEST_ROOT/$site"
done

echo "web-dist: 三个站点发布完成"
