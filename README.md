# CLIProxy 账号门户

面向 CLIProxyAPI + CPA-Manager-Plus 的轻量账号注册、人工审批与 API Key 自助门户。单个 Go 可执行文件、服务端渲染页面、SQLite WAL；运行时不需要 Node.js、Redis 或独立数据库。

> **重要风险提示**：按当前需求，门户和模型 API 均使用公网 HTTP。手机号、姓名、密码、会话 Cookie 和 API Key 都可能被链路上的第三方窃听或篡改。系统已经启用密码哈希、CSRF、防点击劫持等应用层保护，但这些不能替代 HTTPS。建议至少限制可信网络/VPN；一旦条件允许，应在反向代理上启用 HTTPS。

## 已实现功能

- 手机号、姓名、密码注册；管理员人工批准或填写原因拒绝；被拒用户可修正后重提。
- Argon2id 密码哈希；管理员看不到密码；人工签发 15 分钟一次性重置码。
- 每位批准用户一个有效 `cpa_portal_` API Key；完整 Key 只显示一次；CPAMP 中保存 Key，门户只保存 SHA-256 和末四位。
- 用户用密码确认后领取、轮换或撤销 Key；撤销后可立即重新领取，已有 Key 可随时轮换。
- CPAMP Full/Manager Server 提供按用户 7 天、30 天和自定义区间用量、模型列表、全局统计与健康状态。
- 管理员可审批、停用、恢复、修改手机号、重置密码、彻底删除/匿名化账号、控制开放注册、发布版本化使用规则并查看审计日志。
- CPAMP 故障时不会假报撤销成功；待停用/删除任务每 30 秒重试；每 5 分钟对账，尊重在 CPAMP 中手工删除的 Key，不会自动补回。

现有手工 Key 不导入用户账号，也不在门户展示；全局用量中仍可能包含它们。门户变更 Key 时采用“读取—合并—写回”，会保留 CPAMP 中已有的手工 Key。极少数同时修改可能产生竞争，因此避免在同一秒由门户和 CPAMP 两边同时改 Key。

## 部署前提

- Linux 主机已安装 Docker Engine 和 Docker Compose。
- CLIProxyAPI 已在 `8317` 端口运行。
- CPA-Manager-Plus 已以 **Full/Manager Server** 模式在 `18317` 端口运行，并能访问 CLIProxyAPI 管理接口。
- 准备 CPAMP 管理员 Key。它只挂载进门户容器，不会显示在网页或日志中。

CLIProxyAPI 的 Management Center 是管理 UI，本身不是代理；管理 Key 与客户端 API Key 不同。上游说明也要求远程管理时显式允许 remote management。参考 [CLI Proxy API Management Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/tree/main) 与 [CPA-Manager-Plus](https://github.com/seakee/CPA-Manager-Plus)。

## 快速部署

### 低内存服务器：使用预编译二进制（推荐）

如果 VPS 无法完成 Go/Docker 多阶段构建，不要在服务器运行 `docker compose build`。在另一台机器执行：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o dist/portal-linux-amd64 ./cmd/portal
```

ARM64 服务器把 `GOARCH` 和文件名改为 `arm64`。将项目与 `dist/` 一并上传后，执行：

```bash
chmod 755 dist/portal-linux-*
mkdir -p data
chown 10001:10001 data
docker compose -f compose.prebuilt.yaml up -d
```

该方式只拉取很小的 Alpine 运行镜像，不在 VPS 编译。`.env` 中 x86_64 使用 `PORTAL_BINARY=portal-linux-amd64`，ARM64 使用 `PORTAL_BINARY=portal-linux-arm64`。

1. 创建本地配置：

   ```bash
   cp .env.example .env
   ```

   编辑 `.env`，把域名替换为实际值：

   ```dotenv
   PORTAL_EXTERNAL_URL=http://portal.example.com:18080
   CPA_API_BASE_URL=http://portal.example.com:8317
   CPAMP_BASE_URL=http://host.docker.internal:18317
   ```

   `CPA_API_BASE_URL` 是用户最终复制到客户端的公网地址；`CPAMP_BASE_URL` 是门户容器访问 CPAMP 的内部地址。如果两个现有容器不向宿主机暴露 `18317`，请将门户加入它们的 Docker 网络，并把该值改成 CPAMP 容器名。

2. 创建秘密文件：

   ```bash
   mkdir -p secrets
   openssl rand -hex 32 > secrets/portal_app_secret
   printf '%s' '你的_CPAMP_管理员_Key' > secrets/cpamp_admin_key
   chown 10001:10001 secrets/*
   chmod 600 secrets/*
   ```

3. 构建并启动：

   ```bash
   docker compose up -d --build
   docker compose ps
   curl http://127.0.0.1:18080/healthz
   ```

4. 创建首位管理员。密码至少 14 位，且不能包含手机号：

   ```bash
   printf '%s' '请换成强初始密码' > secrets/initial_admin_password
   chown 10001:10001 secrets/initial_admin_password
   chmod 600 secrets/initial_admin_password
   docker compose run --rm \
     -v "$PWD/secrets/initial_admin_password:/run/secrets/initial_admin_password:ro" \
     cliproxy-portal create-admin \
     --phone 13800138000 --name 系统管理员 \
     --password-file /run/secrets/initial_admin_password
   rm secrets/initial_admin_password
   ```

5. 浏览器访问 `http://你的域名:18080/login`。管理员可在“使用规则”页面调整规则和开放注册状态。

模型调用仍直接使用 `http://你的域名:8317/v1`，门户不代理模型流量。

## 日常命令

```bash
docker compose logs -f --tail=100 cliproxy-portal
docker compose restart cliproxy-portal
docker compose pull
docker compose up -d --build
```

SQLite 位于 Compose 命名卷 `portal-data`。当前版本按需求不自动备份。删除该卷会永久丢失账号、Key 映射和审计数据：

```bash
# 危险：仅在确实要彻底清空门户时执行
docker compose down -v
```

## 配置

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `PORTAL_LISTEN_ADDR` | `:18080` | 门户监听地址 |
| `PORTAL_DATABASE_PATH` | `/data/portal.db` | SQLite 路径 |
| `PORTAL_EXTERNAL_URL` | `http://localhost:18080` | 门户对外地址 |
| `CPA_API_BASE_URL` | `http://localhost:8317` | 展示给用户的模型 API 地址 |
| `CPAMP_BASE_URL` | `http://host.docker.internal:18317` | CPAMP Manager Server 地址 |
| `CPAMP_ADMIN_KEY_FILE` | `/run/secrets/cpamp_admin_key` | CPAMP 管理 Key 文件 |
| `PORTAL_APP_SECRET_FILE` | `/run/secrets/portal_app_secret` | 至少 32 字符的会话/CSRF 密钥文件 |
| `PORTAL_RECONCILE_INTERVAL` | `5m` | Key 对账间隔 |
| `PORTAL_PENDING_RETRY` | `30s` | 待撤销任务重试间隔 |
| `PORTAL_USAGE_CACHE_TTL` | `2m` | CPAMP 用量查询的进程内短缓存；重启即清空 |
| `PORTAL_REGISTRATION_OPEN` | `true` | 仅首次建库时的注册开关默认值 |
| `PORTAL_TRUST_PROXY_HEADERS` | `false` | 是否信任 `X-Forwarded-For`；直连公网时保持 false |

## 本地验证

```bash
go test ./...
go vet ./...
go build ./cmd/portal
```

运行时资源上限在 `compose.yaml` 中设为 0.75 CPU、256 MiB 内存。Argon2id 登录/改密会短时使用较多内存，这是有意的密码保护成本。
