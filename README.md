# tg-channel

Telegram 消息转发服务。登录 Telegram 账号，选择要关注的对话，新消息推送到手机（ntfy / Bark），可以从通知直接回复。消息正文不落库。

## 功能

- 账号密码登录，管理员和普通用户
- 扫码添加 Telegram 账号，支持两步验证
- 按对话开关监控
- 推送到 ntfy（iOS / Android）或 Bark（iOS）
- 通知上带"回复"按钮，打开免登录回复页，每条消息最多回复 3 次，链接 24 小时有效
- 消息记录只保留元数据（对话、发送者、时间、推送结果），默认 7 天
- 管理员可以创建用户、重置密码、删除用户
- 手机端自适应

## 技术栈

Go、gin、html/template、HTMX、Pico.css、SQLite、gotd/td。单个二进制，静态资源内嵌。

```
cmd/server          入口
cmd/tgcheck         诊断工具
internal/config     配置
internal/db         模型与数据库
internal/tgclient   Telegram 连接、登录、监听、回复
internal/push       消息入队、推送、ntfy 用户管理、数据清理
internal/reply      回复链接 token
internal/web        路由、页面、登录态
deploy/             Dockerfile、ntfy 配置、Nginx 配置
```

## 环境要求

- 服务器：Docker、Docker Compose、Nginx
- 一个域名，加两条子域名解析（ntfy、bark）到服务器，HTTPS 证书（certbot 即可）
- Telegram API 凭据：在 https://my.telegram.org/apps 申请一对 `api_id` / `api_hash`

## 本地运行

```bash
cp .env.example .env     # 填 SESSION_SECRET、MASTER_KEY、ADMIN_PASSWORD、TG_API_ID、TG_API_HASH
go run ./cmd/server      # http://127.0.0.1:8080
go test ./...
```

## 部署

```bash
cp .env.example .env                 # 填好配置
docker compose up -d --build         # 起 app、ntfy、bark 三个容器
docker compose exec -e NTFY_PASSWORD=<密码> ntfy ntfy user add --role=admin tg-service   # 密码同 .env 的 NTFY_PASSWORD
```

然后按 [deploy/nginx/README.md](deploy/nginx/README.md) 配反代和证书。更新版本：`git pull && docker compose up -d --build app`。

## 配置

| 变量 | 说明 | 默认 |
|---|---|---|
| `ADDR` | 监听地址 | `:8080` |
| `BASE_PATH` | 挂在子路径时填，如 `/tg` | 空 |
| `PUBLIC_URL` | 对外完整地址（含 `BASE_PATH`），设置后通知才有回复按钮 | 空 |
| `TRUSTED_PROXIES` | 可信反向代理地址 | `127.0.0.1,::1` |
| `SESSION_SECRET` | 登录 cookie 和回复链接的签名密钥 | 必填 |
| `MASTER_KEY` | Telegram 会话加密密钥，换了要重新扫码 | 必填 |
| `ADMIN_USER` / `ADMIN_PASSWORD` | 首次启动创建的管理员 | `admin` / 空则不创建 |
| `TG_API_ID` / `TG_API_HASH` | Telegram API 凭据 | 必填 |
| `NTFY_URL` | ntfy 地址 | `https://ntfy.sh` |
| `NTFY_USER` / `NTFY_PASSWORD` | ntfy 服务账号（admin 角色） | 空则匿名发布 |
| `NTFY_BASE_URL` | 传给 ntfy 容器的对外地址，和 `NTFY_URL` 相同 | 空 |
| `BARK_URL` | Bark 地址 | `https://api.day.app` |
| `RETENTION_DAYS` | 消息元数据保留天数 | `7` |
| `LOG_LEVEL` | 日志级别 | `info` |
| `PPROF` | 在 `127.0.0.1:6060` 开 pprof | `false` |

## 安全与隐私

- 消息正文不写数据库，只在内存里排队推送，推完即释放；重启时未推完的任务标记失败。
- 数据库只有对话、发送者、时间和推送结果，到期自动清理，删除的数据立即清零。
- 回复链接内容在 URL 的 `#` 后面，不会发到服务器，也不进日志；带签名和过期时间。
- 只能回复已收到的消息，不能主动发起会话，每账号 5 秒一条。
- Telegram 会话加密存储，每个会话只有一条连接。
- ntfy 关闭匿名访问，每个用户只能读自己账号的 topic。
- 登录和注册按 IP 限速；重置密码后旧登录全部失效。
- 推送内容经过 ntfy / Bark 服务器和苹果、谷歌推送通道，这几段是明文。

## 许可证

MIT
