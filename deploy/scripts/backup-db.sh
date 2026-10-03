#!/usr/bin/env bash
# ============================================================================
# ClassWatch 数据库备份（Phase 11 / §63）
#
# 用法：
#   deploy/scripts/backup-db.sh
#
# 环境变量（通常直接来自 deploy/.env.production，脚本会自动加载）：
#   POSTGRES_USER / POSTGRES_DB   必填，要备份的库
#   BACKUP_DIR                    必填，备份落地的**宿主机目录**
#   BACKUP_RETENTION              必填，保留最近 N 份（其余按时间删除）
#   COMPOSE_FILE / ENV_FILE       可选，默认 deploy/docker-compose.prod.yml 与 deploy/.env.production
#
# 设计要点（WHY）：
#   1) 用 `pg_dump -Fc`（自定义格式）而不是 SQL 文本：
#      自定义格式是压缩的、可分块恢复、可只恢复某张表，还能用 pg_restore 校验；
#      文本格式在恢复时遇到一个语法错误就会停在半路，留下半截 schema。
#   2) **不要**备份数据卷目录：数据库运行中拷贝文件级快照得到的是损坏的备份
#      （PostgreSQL 的文件在事务边界之间是不一致的）。逻辑备份才是可靠的。
#   3) 先写 .partial 再 mv：避免"备份任务被中断后，目录里留下一个看起来正常、
#      其实是半截的 .dump"。恢复一个坏备份比没有备份更糟 —— 它会让你以为有退路。
#   4) 每份备份配一个 .meta（schema 版本 + PostgreSQL 版本 + sha256）：
#      恢复时用来校验"恢复出来的库和备份时是同一个 schema 版本"。
#   5) 保留 N 份而不是无限保留：磁盘写满会让**数据库先挂**（它需要写 WAL），
#      所以"备份把磁盘撑爆"是备份系统最经典的自杀方式。
#
# 建议用 cron 每日执行（注意 cron 的环境变量极少，请显式写出路径）：
#   30 3 * * * /srv/classwatch/deploy/scripts/backup-db.sh >> /var/log/classwatch-backup.log 2>&1
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(dirname "$SCRIPT_DIR")"

COMPOSE_FILE="${COMPOSE_FILE:-$DEPLOY_DIR/docker-compose.prod.yml}"
ENV_FILE="${ENV_FILE:-$DEPLOY_DIR/.env.production}"
POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"

# .env.production 是我们自己维护的 KEY=VALUE 文件，直接 source 是最简单可靠的方式
# （`set -a` 让所有变量自动导出给子进程）。
if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

# ${VAR:?} 保护：这些变量为空时**必须**立刻失败，而不是把备份写到一个未知位置。
: "${POSTGRES_USER:?POSTGRES_USER 未设置：请在 $ENV_FILE 里配置，或从环境变量传入}"
: "${POSTGRES_DB:?POSTGRES_DB 未设置：请在 $ENV_FILE 里配置，或从环境变量传入}"
: "${BACKUP_DIR:?BACKUP_DIR 未设置：备份必须落在与数据库卷**不同的**磁盘/挂载点上}"
: "${BACKUP_RETENTION:?BACKUP_RETENTION 未设置：例如 7（保留最近 7 份）}"

case "$BACKUP_RETENTION" in
  ''|*[!0-9]*)
    printf 'FATAL BACKUP_RETENTION 必须是正整数，当前为 %s\n' "$BACKUP_RETENTION" >&2
    exit 2
    ;;
esac
if [ "$BACKUP_RETENTION" -lt 1 ]; then
  printf 'FATAL BACKUP_RETENTION 至少为 1（0 意味着"备份完立刻删掉"）\n' >&2
  exit 2
fi

# 组装 compose 命令：ENV_FILE 不存在时就不传 --env-file，
# 这样也支持"变量已经由密钥管理器/编排系统注入到环境里"的用法。
COMPOSE=(docker compose)
if [ -n "$ENV_FILE" ] && [ -f "$ENV_FILE" ]; then
  COMPOSE+=(--env-file "$ENV_FILE")
fi
COMPOSE+=(-f "$COMPOSE_FILE")

command -v docker >/dev/null 2>&1 || { printf 'FATAL 找不到 docker\n' >&2; exit 2; }

# 前置检查：数据库容器必须在跑。否则 pg_dump 的报错信息会是一长串 compose 噪音。
if ! "${COMPOSE[@]}" ps --status running --services 2>/dev/null | grep -qx "$POSTGRES_SERVICE"; then
  printf 'FATAL 服务 %s 没有在运行；备份必须在数据库在线时执行。\n' "$POSTGRES_SERVICE" >&2
  printf '      先执行：docker compose -f %s ps\n' "$COMPOSE_FILE" >&2
  exit 1
fi

mkdir -p "$BACKUP_DIR"
# 备份里含全部课堂数据（学生姓名、会话、事件），目录权限收紧到仅属主可读。
chmod 700 "$BACKUP_DIR"

STAMP="$(date -u '+%Y%m%dT%H%M%SZ')"
FINAL="$BACKUP_DIR/classwatch-$STAMP.dump"
PARTIAL="$BACKUP_DIR/.classwatch-$STAMP.dump.partial"

printf '==> 备份 %s/%s → %s\n' "$POSTGRES_USER" "$POSTGRES_DB" "$FINAL"

# --format=custom：压缩 + 可校验 + 可选择性恢复。
# --no-owner/--no-privileges：恢复时不必先创建同名角色，跨环境恢复更省事。
"${COMPOSE[@]}" exec -T "$POSTGRES_SERVICE" \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  --format=custom --no-owner --no-privileges \
  > "$PARTIAL"

# 校验 1：文件魔数必须是 PGDMP。这能挡住"pg_dump 失败但 shell 仍然生成了空文件"
# 以及"把 stderr 的报错信息重定向进了备份文件"这两类典型事故。
MAGIC="$(head -c 5 "$PARTIAL" 2>/dev/null || true)"
if [ "$MAGIC" != "PGDMP" ]; then
  printf 'FATAL 备份文件不是合法的 PostgreSQL 自定义格式归档（魔数=%s）\n' "${MAGIC:-<空>}" >&2
  printf '      已保留现场：%s\n' "$PARTIAL" >&2
  exit 1
fi

SIZE="$(wc -c < "$PARTIAL" | tr -d ' ')"
if [ "$SIZE" -lt 1024 ]; then
  printf 'FATAL 备份只有 %s 字节，明显不完整\n' "$SIZE" >&2
  exit 1
fi

# 记下 schema 版本：恢复时用它校验"恢复出来的库确实是这份备份的状态"。
# coalesce 到 0 是为了兼容"全新库、还没跑过任何迁移"的情况。
SCHEMA_VERSION="$("${COMPOSE[@]}" exec -T "$POSTGRES_SERVICE" \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc \
  'SELECT coalesce(max(version), 0) FROM schema_migrations' | tr -d ' \r\n')"
PG_VERSION="$("${COMPOSE[@]}" exec -T "$POSTGRES_SERVICE" \
  postgres --version | tr -d '\r\n')"

# sha256：跨机器校验备份完整性的最小手段（配合异地拷贝使用）。
if command -v sha256sum >/dev/null 2>&1; then
  CHECKSUM="$(sha256sum "$PARTIAL" | awk '{print $1}')"
else
  CHECKSUM="$(shasum -a 256 "$PARTIAL" | awk '{print $1}')"
fi

mv "$PARTIAL" "$FINAL"
cat > "$FINAL.meta" <<META
format=custom
created_at=$STAMP
postgres_user=$POSTGRES_USER
postgres_db=$POSTGRES_DB
schema_version=${SCHEMA_VERSION:-0}
postgres_version=$PG_VERSION
sha256=$CHECKSUM
META
chmod 600 "$FINAL" "$FINAL.meta"

printf '    大小：%s 字节\n' "$SIZE"
printf '    schema_version：%s\n' "${SCHEMA_VERSION:-0}"
printf '    sha256：%s\n' "$CHECKSUM"

# 保留最近 N 份。注意 `|| true`：目录里一份备份都没有时 ls 会失败，
# 在 set -o pipefail 下会直接中断脚本 —— 那属于"没什么可清理"的正常情况。
OLD="$(ls -1t "$BACKUP_DIR"/classwatch-*.dump 2>/dev/null | tail -n +$((BACKUP_RETENTION + 1)) || true)"
if [ -n "$OLD" ]; then
  printf '%s\n' "$OLD" | while IFS= read -r f; do
    [ -n "$f" ] || continue
    rm -f -- "$f" "$f.meta"
    printf '==> 已删除超出保留份数的旧备份：%s\n' "$f"
  done
fi

printf '==> 备份完成：%s\n' "$FINAL"
printf '    下一步建议：把 .dump 与 .meta 一起复制到**另一台机器/另一个磁盘**。\n'
printf '    只在本机保留备份 = 磁盘坏掉时数据与备份一起消失。\n'
