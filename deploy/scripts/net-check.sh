#!/usr/bin/env bash
# ============================================================================
# ClassWatch 网络可达性与 TURN 检查（任务书 §62 / Phase 11）
#
# 用法：
#   ./net-check.sh api.example.com rtc.example.com
#   ./net-check.sh api.example.com moniter-class-xxxx.livekit.cloud   # LiveKit Cloud
#   ./net-check.sh localhost:8090                                     # 本地开发（只查控制面）
#
# 参数：
#   $1  API 入口 host[:port]（默认 443，走 https；非 443 端口按 http 处理）
#   $2  媒体面 host[:port]（可选）。LiveKit Cloud 传 <project>.livekit.cloud；
#       自建 LiveKit 传 rtc.<domain>。不传则跳过媒体面相关检查。
#
# 退出码：
#   0 = 没有 FAIL（WARN 不算失败，它表示"这里需要人来判断"）
#   1 = 至少一项 FAIL
#   2 = 用法/依赖错误（例如缺少 openssl）
#
# 为什么**不**用 set -e：
#   这个脚本的职责是"把每一项都测完并如实报告"。任何一项失败都不应该中断后面的
#   检查（否则一次 DNS 配错会让你看不到 TLS 也是坏的）。所以用 `set -uo pipefail`
#   加显式判断，而不是 `set -e`。
#
# 为什么域名用占位符时要**失败**而不是"跳过"：
#   把占位域名当成"没配置所以跳过"，会让一条 `./net-check.sh api.example.com`
#   在什么都没验证的情况下退出码为 0 —— 这正是"假装成功"。占位域名一律记 FAIL，
#   并明确告诉你它不可能解析。
# ============================================================================
set -uo pipefail

# ---------------------------------------------------------------------------
# 输出helper
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YEL=$'\033[33m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_RED=''; C_GRN=''; C_YEL=''; C_DIM=''; C_OFF=''
fi

PASS=0; WARN=0; FAIL=0
pass() { printf '  %s✓%s %s\n' "$C_GRN" "$C_OFF" "$*"; PASS=$((PASS + 1)); }
warn() { printf '  %s!%s %s\n' "$C_YEL" "$C_OFF" "$*"; WARN=$((WARN + 1)); }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_OFF" "$*"; FAIL=$((FAIL + 1)); }
info() { printf '    %s%s%s\n' "$C_DIM" "$*" "$C_OFF"; }
section() { printf '\n%s\n' "$*"; printf '%s\n' "------------------------------------------------------------------------"; }

need() {
  command -v "$1" >/dev/null 2>&1 && return 0
  printf '%sFATAL%s 缺少必需命令：%s\n' "$C_RED" "$C_OFF" "$1" >&2
  exit 2
}

usage() {
  sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

[ $# -ge 1 ] || usage
case "${1:-}" in -h|--help|help) usage ;; esac

API_TARGET="$1"
RTC_TARGET="${2:-}"

# ---------------------------------------------------------------------------
# 参数解析：host[:port]
# ---------------------------------------------------------------------------
split_target() { # $1=host[:port]  $2=默认端口 -> 设置 T_HOST / T_PORT
  local raw="$1" def="$2"
  case "$raw" in
    *:*)
      T_HOST="${raw%%:*}"
      T_PORT="${raw##*:}"
      ;;
    *)
      T_HOST="$raw"
      T_PORT="$def"
      ;;
  esac
}

split_target "$API_TARGET" 443
API_HOST="$T_HOST"; API_PORT="$T_PORT"
if [ -n "$RTC_TARGET" ]; then
  split_target "$RTC_TARGET" 443
  RTC_HOST="$T_HOST"; RTC_PORT="$T_PORT"
else
  RTC_HOST=""; RTC_PORT=""
fi

# 443/8443 视为 TLS，其余当作明文 HTTP（本地开发场景）。
api_scheme='http'; [ "$API_PORT" = "443" ] && api_scheme='https'
API_BASE="$api_scheme://$API_HOST"
[ "$API_PORT" = "443" ] || API_BASE="$api_scheme://$API_HOST:$API_PORT"
[ -n "$RTC_HOST" ] && RTC_BASE="https://$RTC_HOST"
[ -n "$RTC_HOST" ] && [ "$RTC_PORT" != "443" ] && RTC_BASE="https://$RTC_HOST:$RTC_PORT"

# 占位符识别：这些值**不可能**解析成功，必须显式报 FAIL。
is_placeholder() {
  case "$1" in
    ''|*example.com*|*example.invalid*|*CHANGE_ME*|*'<'*|*your-domain*) return 0 ;;
    *) return 1 ;;
  esac
}

need curl
# openssl 只在做 TLS 检查时才需要，因此放到那里再判断。

printf 'ClassWatch 网络可达性检查\n'
printf '  API  入口 : %s\n' "$API_BASE"
printf '  媒体面    : %s\n' "${RTC_BASE:-<未提供，跳过媒体面检查>}"
printf '  时间      : %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

# ===========================================================================
# 1. DNS 解析
# ===========================================================================
dns_lookup() { # $1=host $2=A|AAAA
  local host="$1" type="$2"
  if command -v dig >/dev/null 2>&1; then
    dig +short +time=3 +tries=1 "$type" "$host" 2>/dev/null
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c '
import socket, sys
host, typ = sys.argv[1], sys.argv[2]
fam = socket.AF_INET if typ == "A" else socket.AF_INET6
try:
    for res in socket.getaddrinfo(host, None, fam, socket.SOCK_STREAM):
        print(res[4][0])
except OSError:
    pass
' "$host" "$type"
  fi
}

check_dns() { # $1=label $2=host
  local label="$1" host="$2" a aaaa
  if is_placeholder "$host"; then
    fail "$label 的域名是占位符：$host"
    info "example.com / CHANGE_ME 这类值不会解析到你的服务器。"
    info "请传入真实域名（例如 api.school.edu.cn），或本地检查用 localhost:8090。"
    return
  fi
  a="$(dns_lookup "$host" A | grep -E '^[0-9]{1,3}(\.[0-9]{1,3}){3}$' | tr '\n' ' ')"
  aaaa="$(dns_lookup "$host" AAAA | grep -E ':' | tr '\n' ' ')"
  if [ -n "$a" ] || [ -n "$aaaa" ]; then
    pass "$label DNS 解析成功：A=[${a% }] AAAA=[${aaaa% }]"
    [ -n "$aaaa" ] || info "没有 AAAA 记录：只有 IPv6 的客户端（部分手机热点）将无法连接。"
  else
    fail "$label DNS 解析不到任何 A/AAAA 记录"
    info "排查：dig $host A；确认域名的 NS 已生效、记录指向本机公网 IP。"
  fi
}

section "1. DNS 解析"
check_dns "API" "$API_HOST"
[ -n "$RTC_HOST" ] && check_dns "媒体面" "$RTC_HOST"

# ===========================================================================
# 2. TLS 443 握手与证书有效期
# ===========================================================================
epoch_of() { # 把 openssl 的 notAfter（如 "Jul  3 21:10:00 2026 GMT"）转成 epoch
  local s="$1" e
  if e="$(date -d "$s" +%s 2>/dev/null)"; then printf '%s' "$e"; return 0; fi
  if e="$(date -j -f '%b %d %T %Y %Z' "$s" +%s 2>/dev/null)"; then printf '%s' "$e"; return 0; fi
  printf ''
}

check_tls() { # $1=label $2=host $3=port
  local label="$1" host="$2" port="$3" out rc cert subj san notafter end_epoch now days
  if is_placeholder "$host"; then
    fail "$label TLS：域名是占位符，跳过握手（参见上面的 DNS 说明）"
    return
  fi
  [ "$port" = "443" ] || { info "$label 端口是 ${port}（非 443），按明文处理，跳过 TLS 检查"; return; }

  out="$(openssl s_client -connect "$host:$port" -servername "$host" -verify 5 -verify_return_error </dev/null 2>&1)"
  rc=$?
  if [ $rc -ne 0 ]; then
    fail "$label TLS 握手或证书校验失败（openssl 退出码 ${rc}）"
    printf '%s\n' "$out" | grep -Ei 'verify|error|alert' | head -5 | while IFS= read -r l; do info "$l"; done
    info "常见原因：证书还没签发（Caddy 首次启动要几秒）、域名未指向本机、或中间有防火墙拦了 443。"
    return
  fi
  pass "$label TLS 握手成功且证书链校验通过（含主机名校验）"

  cert="$(printf '%s\n' "$out" | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p')"
  if [ -z "$cert" ]; then
    warn "$label 拿不到证书内容，无法检查有效期"
    return
  fi
  subj="$(printf '%s' "$cert" | openssl x509 -noout -subject 2>/dev/null | sed 's/^subject=//')"
  san="$(printf '%s' "$cert" | openssl x509 -noout -ext subjectAltName 2>/dev/null | tail -n +2 | tr -d ' ' | tr '\n' ' ')"
  notafter="$(printf '%s' "$cert" | openssl x509 -noout -enddate 2>/dev/null | sed 's/^notAfter=//')"
  info "subject: $subj"
  info "SAN    : ${san:-<无>}"
  info "notAfter: $notafter"

  # 证书里的 SAN 是否覆盖了我们请求的主机名（s_client 的 -verify_return_error
  # 只校验证书链，不校验主机名，所以这里必须自己确认一次）。
  local matched=0
  case "$san" in
    *"DNS:$host"*) matched=1 ;;
  esac
  if [ $matched -eq 0 ]; then
    # 通配符证书：把主机名的第一段替换成 * 再比一次
    local wild="*.${host#*.}"
    case "$san" in *"DNS:$wild"*) matched=1 ;; esac
  fi
  if [ $matched -eq 1 ]; then
    pass "$label 证书覆盖 $host"
  else
    fail "$label 证书 SAN 不包含 $host —— 浏览器会报 NET::ERR_CERT_COMMON_NAME_INVALID"
    info "上面的 SAN 列表就是这张证书实际覆盖的名字。"
  fi

  end_epoch="$(epoch_of "$notafter")"
  if [ -n "$end_epoch" ]; then
    now="$(date +%s)"
    days=$(( (end_epoch - now) / 86400 ))
    if [ "$days" -lt 0 ]; then
      fail "$label 证书已过期（$((-days)) 天前）"
    elif [ "$days" -lt 14 ]; then
      fail "$label 证书只剩 $days 天：自动续期可能已经失败，请立刻检查 Caddy 日志"
      info "docker compose -f deploy/docker-compose.prod.yml logs caddy | grep -i acme"
    elif [ "$days" -lt 30 ]; then
      warn "$label 证书还剩 $days 天（Caddy 通常在到期前 30 天续期，注意观察）"
    else
      pass "$label 证书有效期剩余 $days 天"
    fi
  fi
}

section "2. TLS 443 握手与证书有效期"
if command -v openssl >/dev/null 2>&1; then
  if [ "$api_scheme" = "https" ]; then
    check_tls "API" "$API_HOST" "$API_PORT"
  else
    info "API 目标是明文（${API_BASE}），跳过 TLS 检查（本地开发场景）"
  fi
  [ -n "$RTC_HOST" ] && [ "$RTC_PORT" = "443" ] && check_tls "媒体面" "$RTC_HOST" "$RTC_PORT"
else
  warn "本机没有 openssl，跳过 TLS 检查（不视为失败，但证书状态未知）"
fi

# ===========================================================================
# 3. HTTP→HTTPS 跳转（只在真实 443 目标上做）
# ===========================================================================
section "3. HTTP→HTTPS 跳转"
if [ "$api_scheme" = "https" ] && ! is_placeholder "$API_HOST"; then
  redirect="$(curl -sS -m 10 -o /dev/null -w '%{http_code} %{redirect_url}' "http://$API_HOST/.well-known/acme-check" 2>/dev/null || true)"
  code="${redirect%% *}"; url="${redirect##* }"
  case "$code" in
    301|302|307|308)
      case "$url" in
        https://*) pass "http://$API_HOST → $code $url" ;;
        *) warn "http 返回 ${code}，但跳转目标不是 https：$url" ;;
      esac
      ;;
    000|'') warn "无法连接 http://$API_HOST:80（可能只开了 443，或云防火墙没放行 80）"
           info "只开 443 也能用，但 ACME HTTP-01 续期会失败；确认你用的是 DNS-01 或 TLS-ALPN-01。" ;;
    *) warn "http://$API_HOST 返回 ${code}（期望 308 跳转到 https）" ;;
  esac
else
  info "跳过（本地/占位目标没有可验证的 80 端口跳转）"
fi

# ===========================================================================
# 4. 控制面探针：/healthz 与 /readyz
# ===========================================================================
section "4. 控制面探针"
code="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$API_BASE/healthz" 2>/dev/null || true)"
case "$code" in
  200) pass "GET $API_BASE/healthz → 200" ;;
  000|'') fail "GET $API_BASE/healthz 连不上（curl 无法建立连接）"
          info "检查：DNS、防火墙 443、Caddy 是否在跑（docker compose ps caddy）。" ;;
  *) fail "GET $API_BASE/healthz → ${code}（期望 200）" ;;
esac

ready_body="$(curl -sS -m 10 -w '\n%{http_code}' "$API_BASE/readyz" 2>/dev/null || true)"
ready_code="$(printf '%s' "$ready_body" | tail -n 1)"
ready_json="$(printf '%s' "$ready_body" | sed '$d')"
case "$ready_code" in
  200)
    pass "GET $API_BASE/readyz → 200（依赖全部就绪）"
    ;;
  503)
    fail "GET $API_BASE/readyz → 503（API 活着但依赖没就绪：数据库/Redis/LiveKit 有一项不通）"
    ;;
  000|'')
    fail "GET $API_BASE/readyz 连不上"
    ;;
  *)
    fail "GET $API_BASE/readyz → ${ready_code}（期望 200）"
    ;;
esac
[ -n "$ready_json" ] && info "readyz 原始响应：$ready_json"

# ===========================================================================
# 5. 实时通道（WSS / WebSocket Upgrade）
# ===========================================================================
# 这一段是"反代有没有正确转发 Upgrade/Connection 头"的直接验证：
#   - 控制面 /ws/student 在没有会话时**也会先完成 101**，然后立刻发一个关闭帧
#     （见 services/api/internal/httpapi/ws.go：reject 只在"握手失败"时才回
#     HTTP 错误码）。因此拿到 101 = Upgrade 头被完整转发；
#     如果代理把 Upgrade 丢了，这里会看到 200/401 这类普通 HTTP 响应 —— 那就是
#     典型的"实时通道静默降级"。
#   - LiveKit 的信令端点需要鉴权，401 就是"可达且说话正常"的证明；
#     连不上或 404/502 才是问题。
ws_probe() { # $1=label $2=url $3=期望状态列表(逗号分隔)
  local label="$1" url="$2" expected="$3" key first
  key="$(head -c 16 /dev/urandom | base64 | tr -d '\n')"
  first="$(curl -sS -i -m 8 --http1.1 -N \
      -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
      -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $key" \
      "$url" 2>/dev/null | head -n 1 | tr -d '\r')"
  local status
  status="$(printf '%s' "$first" | awk '{print $2}')"
  if [ -z "$status" ]; then
    fail "$label $url 无响应（连接被拒/超时/DNS 失败）"
    return
  fi
  case ",$expected," in
    *",$status,"*)
      pass "$label $url → $first"
      ;;
    *)
      fail "$label $url → ${first}（期望 ${expected}）"
      info "如果这里是 200/401 而不是 101：反代很可能没有转发 Upgrade/Connection 头，"
      info "实时通道已静默降级为轮询。检查 deploy/Caddyfile 的 websocket_proxy 片段。"
      ;;
  esac
}

section "5. 实时通道（WebSocket Upgrade）"
ws_probe "控制面" "$API_BASE/ws/student" "101"
if [ -n "$RTC_HOST" ] && ! is_placeholder "$RTC_HOST"; then
  ws_probe "媒体面信令" "$RTC_BASE/rtc" "101,401,403"
else
  info "媒体面：未提供真实域名，跳过信令握手"
fi

# ===========================================================================
# 6. LiveKit 服务端接口（Twirp / HTTPS 443）
# ===========================================================================
section "6. LiveKit 服务端接口（Twirp）"
if [ -n "$RTC_HOST" ] && ! is_placeholder "$RTC_HOST"; then
  # 不带凭据地打 RoomService：401/403 = 端点存在且按契约要求鉴权（好）；
  # 404 = 路径或区域写错；000 = 网络不通。这样不需要把 API secret 交给这个脚本。
  twirp="$(curl -sS -m 10 -o /tmp/netcheck_twirp.$$ -w '%{http_code}' \
      -X POST -H 'Content-Type: application/json' -d '{}' \
      "$RTC_BASE/twirp/livekit.RoomService/ListRooms" 2>/dev/null || true)"
  twirp_body="$(head -c 200 /tmp/netcheck_twirp.$$ 2>/dev/null || true)"
  rm -f /tmp/netcheck_twirp.$$
  case "$twirp" in
    200) warn "Twirp ListRooms 未带凭据却返回 200（意外：请确认这个端点是否被公开暴露）" ;;
    401|403)
      pass "Twirp POST $RTC_BASE/twirp/livekit.RoomService/ListRooms → ${twirp}（端点存在且要求鉴权）"
      info "响应：${twirp_body:-<空>}"
      ;;
    404) fail "Twirp 返回 404：端点路径或区域不对"
         info "LiveKit Cloud 的区域域（<region>.livekit.cloud）与项目域可能不同，请核对 LIVEKIT_API_URL。" ;;
    000|'') fail "Twirp 连不上（HTTPS 443 不可达）"
            info "后端 api 容器需要能出网访问 $RTC_HOST:443，否则 /readyz 的 livekit 项不会 ok。" ;;
    *) fail "Twirp → ${twirp}（期望 401/403）" ;;
  esac
else
  info "跳过（未提供媒体面域名）"
fi

# ===========================================================================
# 7. UDP 出站可用性（自建 SFU 必需；LiveKit Cloud 时是"是否需要 TURN"的关键线索）
# ===========================================================================
section "7. UDP 出站可用性 / NAT 类型"
info "说明：媒体包走 UDP。用 LiveKit Cloud 时，UDP 直连失败会由 TURN/TLS(443) 兜底；"
info "      自建 SFU 时，UDP 50000-60000 出站被封就必须自己部署 coturn，否则课堂只能同网段用。"
if command -v python3 >/dev/null 2>&1; then
  stun_out="$(python3 - <<'PY' 2>&1 || true
import os, socket, struct, sys

# RFC 5389 Binding Request：12 字节头（type/len/cookie/txid）。
# 用**公网 STUN 服务器**是否回包来判断"这台机器能不能发出并收到 UDP"，
# 并顺手解析 XOR-MAPPED-ADDRESS，让运维看到自己实际被 NAT 映射成什么地址。
servers = [("stun.l.google.com", 19302), ("stun.cloudflare.com", 3478)]
txid = os.urandom(12)
req = struct.pack("!HHI", 0x0001, 0x0000, 0x2112A442) + txid

for host, port in servers:
    try:
        addr = socket.getaddrinfo(host, port, socket.AF_INET, socket.SOCK_DGRAM)[0][4]
    except OSError as e:
        print("SKIP %s: DNS 解析失败 (%s)" % (host, e)); continue
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(3)
    try:
        s.sendto(req, addr)
        data, _ = s.recvfrom(2048)
    except OSError as e:
        print("FAIL %s: 无响应 (%s)" % (host, e)); s.close(); continue
    s.close()
    if len(data) < 20 or struct.unpack("!H", data[0:2])[0] != 0x0101:
        print("FAIL %s: 收到非 STUN 响应" % host); continue
    # 解析属性，找 XOR-MAPPED-ADDRESS(0x0020)
    mapped = None
    i = 20
    while i + 4 <= len(data):
        atype, alen = struct.unpack("!HH", data[i:i+4]); i += 4
        if atype == 0x0020 and alen >= 8:
            fam = data[i+1]
            xport = struct.unpack("!H", data[i+2:i+4])[0] ^ 0x2112
            if fam == 0x01:
                xip = bytes(b ^ c for b, c in zip(data[i+4:i+8], b"\x21\x12\xa4\x42"))
                mapped = "%d.%d.%d.%d:%d" % (xip[0], xip[1], xip[2], xip[3], xport)
        i += alen + ((4 - alen % 4) % 4)
    print("OK %s -> mapped=%s" % (host, mapped or "?"))

# 顺带对 50000-60000 里的常见端口做一次"发了包"的检查（不会有回包，因此只报告
# 是否能在本地把包发出去 —— 真正的结论必须来自一次真实课堂，见文档）。
try:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(1)
    s.connect(("1.1.1.1", 50000))
    s.send(b"\x00")
    print("SEND 50000/udp: 本机允许向该端口发出 UDP 包（无回包是正常的）")
    s.close()
except OSError as e:
    print("SEND 50000/udp FAIL: %s" % e)
PY
)"
  # 用 here-string 而不是管道：管道会让 while 跑在子 shell 里，计数器就丢了。
  udp_ok=0
  while IFS= read -r line; do
    case "$line" in
      OK*)
        udp_ok=$((udp_ok + 1))
        pass "UDP 出站可用（STUN 有回包）：${line#OK }"
        ;;
      SKIP*) info "跳过：${line#SKIP }" ;;
      # 单个公网 STUN 无响应很常见（被墙/限速），因此只记 INFO；
      # "所有 STUN 都无响应"才是需要解释的结论，见下面的汇总。
      FAIL*) info "STUN 无响应：${line#FAIL }" ;;
      SEND*) info "${line#SEND }" ;;
      *)     info "$line" ;;
    esac
  done <<EOF
$stun_out
EOF
  if [ "$udp_ok" -eq 0 ]; then
    warn "所有 STUN 探测都没有回包：UDP 出站很可能被完全阻断。"
    info "用 LiveKit Cloud 时，TURN/TLS(443) 就是唯一的兜底路径，必须在真实受限网络里实测（见文档）。"
    info "自建 SFU 时这种情况**必须**自己部署 coturn（TURN over TLS 443），否则课堂只能同网段使用。"
  fi
  info "⚠️ 只靠 STUN 无法证明 50000-60000 这个**区间**可用。要验证媒体面真的工作，"
  info "   必须做一次真实课堂，并在 chrome://webrtc-internals 里确认候选类型（见文档 TURN 章节）。"
else
  warn "本机没有 python3，无法做 STUN 探测（UDP 状态未知）"
  info "替代：nc -u -z -v <rtc-host> 50000（UDP 的 'succeeded' 不代表对端有响应，仅供参考）"
fi

# ===========================================================================
# 汇总
# ===========================================================================
section "汇总"
printf '  通过 %d 项，警告 %d 项，失败 %d 项\n' "$PASS" "$WARN" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '  %s结论：有 %d 项失败，网络尚未就绪。%s\n' "$C_RED" "$FAIL" "$C_OFF"
  exit 1
fi
printf '  %s结论：没有失败项。注意 WARN 项仍需人工确认。%s\n' "$C_GRN" "$C_OFF"
info "本脚本只能证明「路径可达」。课堂能否真的跑起来（尤其是受限网络下的 TURN）"
info "必须用真实浏览器完成一次 relay-only 测试：见 docs/development/deployment.md。"
exit 0
