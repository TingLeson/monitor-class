#!/usr/bin/env bash
# ============================================================================
# ClassWatch 数据库恢复（Phase 11 / §63）
#
# 用法：
#   deploy/scripts/restore-db.sh <备份文件.dump>
#
#   # 真正执行（必须显式确认，恢复是破坏性操作）：
#   CONFIRM=restore deploy/scripts/restore-db.sh /var/backups/classwatch/classwatch-20261003T031500Z.dump
#
# 环境变量：与 backup-db.sh 相同（POSTGRES_USER / POSTGRES_DB / COMPOSE_FILE / ENV_FILE），
#           外加 CONFIRM=restore（缺省时不执行，只打印将要做什么）。
#
# 设计要点（WHY）：
#   1) 默认**不执行**，先打印计划。恢复是破坏性动作，必须有一次显式确认；
#      CI/cron 里误触发一次恢复的代价是整个课堂数据被回滚。
#   2) 拒绝在目标库还有其它连接时恢复。理由不是"礼貌"，而是正确性：
#      服务在线时恢复，会出现"恢复完成后应用又把它改回旧状态"的静默数据损坏
#      ——表面上恢复"成功"了，实际数据是两份状态的混合。
#   3) `--single-transaction`：pg_restore 全程在一个事务里。失败即整体回滚，
#      不会留下"表建了一半"的库 —— 半截 schema 比空库更难救。
#   4) 恢复后立刻校验 schema 版本与备份时记录的一致（.meta 文件），
#      并打印 migrate status 的用法。迁移是**向前兼容**的，所以"备份比镜像旧"
#      是正常情况，跑一次 migrate up 即可；反过来（备份比镜像新）则必须先换回
#      对应版本的镜像，否则新 schema 上的代码路径不存在。
# ============================================================================
set -euo pipefail

usage() {
  sed -n '3,14p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

[ $# -eq 1 ] || usage
DUMP_FILE="$1"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(dirname "$SCRIPT_DIR")"

COMPOSE_FILE="${COMPOSE_FILE:-$DEPLOY_DIR/docker-compose.prod.yml}"
ENV_FILE="${ENV_FILE:-$DEPLOY_DIR/.env.production}"
POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"

if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

: "${POSTGRES_USER:?POSTGRES_USER 未设置：请在 $ENV_FILE 里配置，或从环境变量传入}"
: "${POSTGRES_DB:?POSTGRES_DB 未设置：请在 $ENV_FILE 里配置，或从环境变量传入}"

# ── 1. 备份文件本身是否可信 ────────────────────────────────────────────────
[ -f "$DUMP_FILE" ] || { printf 'FATAL 备份文件不存在：%s\n' "$DUMP_FILE" >&2; exit 1; }
MAGIC="$(head -c 5 "$DUMP_FILE" 2>/dev/null || true)"
if [ "$MAGIC" != "PGDMP" ]; then
  printf 'FATAL %s 不是 PostgreSQL 自定义格式归档（魔数=%s）\n' "$DUMP_FILE" "${MAGIC:-<空>}" >&2
  printf '      如果你的备份是 .sql 文本，请不要用这个脚本（它走 pg_restore）。\n' >&2
  exit 1
fi

META_FILE="$DUMP_FILE.meta"
WANT_VERSION=""
if [ -f "$META_FILE" ]; then
  WANT_VERSION="$(sed -n 's/^schema_version=//p' "$META_FILE" | tr -d ' \r\n')"
  printf '==> 备份元数据 %s\n' "$META_FILE"
  sed 's/^/    /' "$META_FILE"
  # 校验和：只在双方都算过的时候才有意义（老备份可能没有 sha256 行）。
  WANT_SHA="$(sed -n 's/^sha256=//p' "$META_FILE" | tr -d ' \r\n')"
  if [ -n "$WANT_SHA" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      GOT_SHA="$(sha256sum "$DUMP_FILE" | awk '{print $1}')"
    else
      GOT_SHA="$(shasum -a 256 "$DUMP_FILE" | awk '{print $1}')"
    fi
    if [ "$GOT_SHA" != "$WANT_SHA" ]; then
      printf 'FATAL 备份文件 sha256 与 .meta 不一致（文件可能损坏或被改动）\n' >&2
      printf '      meta=%s\n      实际=%s\n' "$WANT_SHA" "$GOT_SHA" >&2
      exit 1
    fi
    printf '==> sha256 校验通过\n'
  fi
else
  printf 'WARN 找不到 %s：将无法校验 schema 版本与完整性\n' "$META_FILE" >&2
fi

COMPOSE=(docker compose)
if [ -n "$ENV_FILE" ] && [ -f "$ENV_FILE" ]; then
  COMPOSE+=(--env-file "$ENV_FILE")
fi
COMPOSE+=(-f "$COMPOSE_FILE")

command -v docker >/dev/null 2>&1 || { printf 'FATAL 找不到 docker\n' >&2; exit 2; }

if ! "${COMPOSE[@]}" ps --status running --services 2>/dev/null | grep -qx "$POSTGRES_SERVICE"; then
  printf 'FATAL 服务 %s 没有在运行\n' "$POSTGRES_SERVICE" >&2
  exit 1
fi

# ── 2. 目标库上不能有别的连接（见文件头第 2 点）────────────────────────────
ACTIVE="$("${COMPOSE[@]}" exec -T "$POSTGRES_SERVICE" \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc \
  "SELECT count(*) FROM pg_stat_activity WHERE datname = '$POSTGRES_DB' AND pid <> pg_backend_pid()" \
  | tr -d ' \r\n')"
if [ "${ACTIVE:-0}" != "0" ]; then
  printf 'FATAL 数据库 %s 上还有 %s 个活动连接，拒绝恢复。\n' "$POSTGRES_DB" "$ACTIVE" >&2
  printf '      恢复期间必须没有写入者，否则恢复出来的数据会是两份状态的混合。\n' >&2
  printf '      请先停掉写入方：\n' >&2
  printf '        docker compose -f %s stop api\n' "$COMPOSE_FILE" >&2
  printf '      恢复并确认无误后再：\n' >&2
  printf '        docker compose -f %s start api\n' "$COMPOSE_FILE" >&2
  printf '      当前连接（前 10 条）：\n' >&2
  "${COMPOSE[@]}" exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c \
    "SELECT pid, usename, application_name, state, backend_start FROM pg_stat_activity WHERE datname = '$POSTGRES_DB' AND pid <> pg_backend_pid() LIMIT 10" >&2 || true
  exit 1
fi

# ── 3. 计划（默认只打印）──────────────────────────────────────────────────
printf '\n将要执行（目标库 %s/%s）：\n' "$POSTGRES_USER" "$POSTGRES_DB"
printf '  pg_restore --clean --if-exists --no-owner --no-privileges --single-transaction\n'
printf '    < %s\n' "$DUMP_FILE"
printf '\n⚠️ 这会**删除并重建**目标库里与备份同名的对象（--clean）。\n'
printf '   生产环境请先做一次"恢复演练"：把备份恢复到一个临时库上验证，再动生产库。\n\n'

if [ "${CONFIRM:-}" != "restore" ]; then
  printf '未设置 CONFIRM=restore，已停止（没有做任何改动）。\n'
  printf '确认无误后重新执行：\n'
  printf '  CONFIRM=restore %s %s\n' "$0" "$DUMP_FILE"
  exit 1
fi

# ── 4. 恢复 ───────────────────────────────────────────────────────────────
printf '==> 开始恢复\n'
"${COMPOSE[@]}" exec -T "$POSTGRES_SERVICE" \
  pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  --clean --if-exists --no-owner --no-privileges --single-transaction \
  < "$DUMP_FILE"

# ── 5. 校验：恢复出来的 schema 版本必须与备份记录一致 ──────────────────────
GOT_VERSION="$("${COMPOSE[@]}" exec -T "$POSTGRES_SERVICE" \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc \
  'SELECT coalesce(max(version), 0) FROM schema_migrations' | tr -d ' \r\n')"
printf '==> 恢复后 schema_version=%s\n' "${GOT_VERSION:-0}"
if [ -n "$WANT_VERSION" ] && [ "${GOT_VERSION:-0}" != "$WANT_VERSION" ]; then
  printf 'FATAL schema 版本不一致：备份=%s，恢复后=%s\n' "$WANT_VERSION" "$GOT_VERSION" >&2
  printf '      恢复**没有**得到预期的结果，请勿启动服务，先排查（磁盘空间？权限？）。\n' >&2
  exit 1
fi
[ -n "$WANT_VERSION" ] && printf '==> schema 版本校验通过\n'

printf '\n==> 恢复完成。接下来：\n'
printf '  1) 如果备份比当前镜像旧（生产很常见，迁移是向前兼容的）——补跑迁移：\n'
printf '       docker compose -f %s run --rm migrate up\n' "$COMPOSE_FILE"
printf '  2) 查看迁移状态（确认没有 unknown migration）：\n'
printf '       docker compose -f %s run --rm migrate status\n' "$COMPOSE_FILE"
printf '  3) 启动服务并检查就绪：\n'
printf '       docker compose -f %s start api\n' "$COMPOSE_FILE"
printf '       curl -sS https://<API_HOST>/readyz\n'
printf '  4) 用 adminctl list-users 确认账号还在，再让一个真实用户登录一次。\n'
printf '\n注意：如果备份比当前镜像**新**（schema_version 更大），不要直接启动旧镜像 ——\n'
printf '      先换回与备份同版本的镜像，否则新 schema 上的代码路径不存在。\n'
