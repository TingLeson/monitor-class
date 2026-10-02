# deploy/nginx（Phase 11 落地）

本地开发不需要反向代理：三个前端由 Vite dev server 提供，`/api` 通过 Vite 的
`server.proxy` 直接转发到 `http://localhost:8080`，浏览器看到的始终是**同源**请求
（这一点很重要：Cookie 会话在跨源场景下会被浏览器拒绝或需要额外的 SameSite=None + Secure）。

生产环境（任务书 §62）需要把入口拆成域名，并统一处理 TLS 与 WebSocket 升级：

```text
Internet
   │
   ▼
Caddy / Nginx
   ├── student.example.com   → apps/student-web 静态产物
   ├── teacher.example.com   → apps/teacher-web 静态产物
   ├── admin.example.com     → apps/admin-web 静态产物
   ├── api.example.com       → services/api（含 /ws 的 WebSocket 升级）
   └── rtc.example.com       → LiveKit（WSS + WebRTC over TCP/TLS）
```

Phase 11 会在这个目录补齐：

1. 三个站点的静态托管与 SPA history fallback（`try_files ... /index.html`）。
2. `api.` 的反代：转发 Cookie、`X-Request-Id`，保留真实客户端 IP（用于 IP 限流）。
3. `/ws/student`、`/ws/teacher` 的 `Upgrade`/`Connection` 头透传与较长的读超时。
4. LiveKit 的 TLS 终结与 TURN/TLS 配置（受限网络下的连通性兜底）。
5. HSTS、安全响应头、上传/请求体大小限制。

> 在这些配置完成并通过**多网络环境**（校园网、家庭 NAT、手机热点、不同 ISP）真实验证之前，
> 不允许声称 WebRTC 网络已经就绪（任务书 §62）。
