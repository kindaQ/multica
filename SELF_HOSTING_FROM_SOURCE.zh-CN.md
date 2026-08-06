# 从源码编译并启动 Multica Self-host

## 1. 环境要求

- Git、Make、OpenSSL
- Docker Engine 或 Docker Desktop
- Docker Compose v2（`docker compose`）

```bash
docker version
docker compose version
```

backend 和 frontend 都在 Docker 中编译，宿主机不需要安装 Go、Node.js 或 pnpm。

## 2. 拉取源码

```bash
git clone git@gitlab.chehejia.com:algo-project/ai-mam/multica-next.git
cd multica-next
git switch master
```

## 3. 编译并启动

```bash
make selfhost-build
```

该命令会自动创建 `.env`、生成基础密钥、编译 backend 和 frontend，并启动 PostgreSQL、backend、frontend。

> 不要使用 `make selfhost`，该命令启动的是官方镜像，不是当前源码。

## 4. 必要配置

### 端口和访问地址

本机部署使用默认值即可：

```dotenv
PORT=8080
FRONTEND_PORT=3000
FRONTEND_ORIGIN=http://localhost:3000
MULTICA_APP_URL=http://localhost:3000
```

端口冲突时修改 `PORT` 和 `FRONTEND_PORT`。

### 飞书 Bot

使用飞书功能必须生成加密密钥：

```bash
openssl rand -base64 32
```

将输出写入 `.env`：

```dotenv
MULTICA_LARK_SECRET_KEY=<生成的密钥>
MULTICA_LARK_HTTP_BASE_URL=
MULTICA_LARK_CALLBACK_BASE_URL=
```

`MULTICA_LARK_SECRET_KEY` 不能随意更换，否则已有 Bot 需要重新绑定。两个 base URL 正常情况下保持为空。

### 登录邮件

正式环境需要配置 Resend 或 SMTP。Resend 配置示例：

```dotenv
APP_ENV=production
RESEND_API_KEY=<Resend API Key>
RESEND_FROM_EMAIL=noreply@example.com
```

本机测试未配置邮件服务时，从 backend 日志查看验证码：

```bash
docker compose -f docker-compose.selfhost.yml logs backend
```

### 应用配置

修改 `.env` 后执行：

```bash
docker compose \
  -f docker-compose.selfhost.yml \
  -f docker-compose.selfhost.build.yml \
  up -d
```

## 5. 验证启动结果

```bash
docker compose -f docker-compose.selfhost.yml ps
curl -fsS http://localhost:8080/readyz
```

正常响应：

```json
{"status":"ok","checks":{"db":"ok","migrations":"ok"}}
```

- Web：<http://localhost:3000>
- API：<http://localhost:8080>

## 6. 启动智能体守护进程

守护进程运行在实际执行任务的电脑上。需要从源码构建 CLI 时，先安装 Go 1.26.1，然后执行：

```bash
make build
./server/bin/multica setup self-host
./server/bin/multica daemon status
```

执行电脑还需要安装并登录至少一个 AI 编程工具，例如 Codex、Claude Code 或 Cursor Agent。

## 7. 后续启动和停止

源码变化后重新编译并启动：

```bash
make selfhost-build
```

停止服务并保留数据：

```bash
make selfhost-stop
```
