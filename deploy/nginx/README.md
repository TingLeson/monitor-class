# deploy/nginx —— 为什么最终没有用 nginx

> **Phase 11 的反向代理用的是 Caddy，配置在 [`deploy/Caddyfile`](../Caddyfile)。**
> 这个目录保留 nginx 方案的说明，原因有两个：任务书 §36 的目录规划里有它，
> 而且"为什么没选它"本身是部署文档里最值得留下的知识之一。

本地开发不需要反向代理：三个前端由 Vite dev server 提供，`/api`、`/ws` 通过 Vite 的
`server.proxy` 转发到后端，浏览器看到的始终是**同源**请求
（这一点很重要：生产也用同源反代，Cookie 的 SameSite 语义因此最简单）。

## 生产拓扑（§62）

```text
Internet
   │
   ▼
Caddy（:80/:443，唯一对公网暴露的容器）
   ├── student.<domain>   → 静态产物（file_server + SPA fallback）+ 同源反代 /api /ws
   ├── teacher.<domain>   → 同上
   ├── admin.<domain>     → 同上
   ├── api.<domain>       → services/api（/api、/ws、探针、LiveKit webhook）
   └── rtc.<domain>       → LiveKit（仅自建时需要；用 LiveKit Cloud 时不需要）
```

## 选 Caddy 而不是 nginx 的三个理由

| 维度      | Caddy                                                                                                                            | nginx                                                                                                                                             |
| --------- | -------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| 证书      | 自动 ACME 签发/续期/HTTP→HTTPS 跳转是**默认行为**                                                                                | 需要 certbot + 定时任务 + reload，任一环漏配就是"证书过期后全线不可用"                                                                            |
| WebSocket | `reverse_proxy` **默认**识别 `Upgrade` 并完成协议切换                                                                            | 必须手写 `proxy_set_header Upgrade $http_upgrade;` 与 `proxy_set_header Connection "upgrade";`，漏写会**静默**降级（本项目的 `/ws/*` 是实时通道） |
| 客户端 IP | 未配置 `trusted_proxies` 时，客户端传来的 `X-Forwarded-For` 会被 Caddy 观察到的对端地址**整个替换**（已实测，见 Caddyfile 注释） | 需要自己决定 `$proxy_add_x_forwarded_for`（追加，会保留客户端伪造的前缀）还是 `$remote_addr`（替换），选错就是限流被绕过或全站共享一个限流桶      |

## 如果你确实要用 nginx：等价配置的要点

以下每一条都对应 Caddyfile 里的一个决定，照抄时请把注释一起抄走：

```nginx
# 1) WebSocket：漏掉这两行 → 浏览器拿到 200 而不是 101，
#    实时通道静默降级；视频/语音可能仍然是好的，因此极易误判成"媒体面故障"。
map $http_upgrade $connection_upgrade { default upgrade; '' close; }

location /ws/ {
    proxy_pass http://api:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 3600s;   # 长连接不要用默认的 60s
    proxy_buffering off;        # 与 Caddy 的 flush_interval -1 等价
}

# 2) 客户端 IP：只信任你自己的边缘代理，且必须是**替换**而不是追加，
#    否则任何人轮换 X-Forwarded-For 就能给自己换一个新的限流桶。
proxy_set_header X-Forwarded-For $remote_addr;
proxy_set_header X-Real-IP $remote_addr;
# 后端侧再把 Caddy/nginx 的容器地址写进 TRUSTED_PROXIES（本项目默认 172.31.0.2/32）。

# 3) 安全响应头 + CSP：逐条理由见 deploy/Caddyfile（照抄即可，但不要漏 Permissions-Policy
#    的 camera=(self) / microphone=(self) / display-capture=(self)，它们是课堂功能的前提）。
add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
add_header X-Content-Type-Options "nosniff" always;
add_header Referrer-Policy "strict-origin-when-cross-origin" always;

# 4) SPA fallback 与缓存：index.html 不缓存，带 hash 的 /assets/ 长缓存。
location / { try_files $uri /index.html; }
location /assets/ { add_header Cache-Control "public, max-age=31536000, immutable"; }
location = /index.html { add_header Cache-Control "no-store"; }

# 5) 请求体上限：与 API 的 HTTP_MAX_BODY_BYTES 保持一致。
client_max_body_size 1m;

# 6) TURN/TLS **不能**用 nginx 反代：TURN over TLS 不是 HTTP。
#    自建时由 coturn 自己监听 5349/tcp 并持有证书（详见 docs/development/deployment.md）。
```

## 不变量（无论用哪个代理都必须满足）

1. `/ws/student`、`/ws/teacher` 能拿到 **101**（`deploy/scripts/net-check.sh` 第 5 节会替你验证）。
2. 客户端 IP 是"代理观察到的地址"，不是请求头里客户端自己写的值。
3. `index.html` 不被缓存，带内容哈希的静态资源长缓存。
4. `camera` / `microphone` / `display-capture` 三个权限位对本站开放。
5. 只有代理一个容器对公网暴露端口（数据库/Redis/API 全部内网）。

> 在这些配置完成并通过**多网络环境**（校园网、家庭 NAT、手机热点、不同 ISP、只放 443 的受限网络）
> 真实验证之前，不允许声称 WebRTC 网络已经就绪（任务书 §62）。
> 测试矩阵与实测记录见 [`docs/development/deployment.md`](../../docs/development/deployment.md)。
