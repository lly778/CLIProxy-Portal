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
- 可选模型网关隐藏 `/v1/models` 中的别名，同时继续转发别名调用；日志可下载包含用户/助手文本及实际工具交互的加密结构化记录。

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

默认情况下，模型调用仍直接使用 `http://你的域名:8317/v1`。需要隐藏模型别名并下载交互记录时，启用下面的可选网关。

## 可选模型网关

门户可以在独立监听地址上运行模型网关。`GET /v1/models`、账户概览和“可用模型”页面都会移除在门户中配置的、仅作为 Codex 别名的模型 ID；其他允许的模型 API 路径原样转发给 CPA，别名请求仍由 CPA 路由。若别名恰好也是一个已启用的真实 Codex 模型名，模型列表会保留该真实名称。CPA 管理路径不会被网关代理。

本机使用本项目配套的 CPA Manager Plus Compose 时，可以叠加 `compose.gateway.yaml`：它把网关发布到宿主机 `8317`，并将门户加入 `cpamp_default` 网络，通过容器名直连 CPA。须先把 CPA 原有的宿主机 `8317` 映射改为仅本机的其他端口（例如 `.env` 中的 `CPA_PORT=127.0.0.1:18319`），再运行 `docker compose -f compose.prebuilt.yaml -f compose.gateway.yaml up -d`；`CPA_API_BASE_URL` 保持用户原来的公网地址，不用改客户端 URL。若 CPA 不在该 Docker 网络或已有其他反代，应按实际拓扑修改网关的内部上游和端口绑定。**不要把 `CPA_UPSTREAM_URL` 指回网关自身**；仅启用环境变量不会自动完成入口切换。

经网关处理的 Responses、Chat Completions、Messages 和 Gemini 对话请求，会提取用户与助手文本，以及实际工具调用、参数和结果，作为结构化交互事件加密保存；系统/开发者指令、推理、工具定义、图片和其他非文本内容不保留。同一用户反复提交的相同事件只保存一次，不同用户之间不共用；下载按每次请求重建当次交互，默认提供 JSON，添加 `?format=txt` 可下载易读文本。旧版独立文件及旧版共用消息记录均不再兼容，并在新版启动时自动清理。用户可下载自己的记录，管理员可下载全局记录。每位用户最多保留最近 100 条，不按保存天数删除；请求或响应原始数据单侧超过 16 MB 时标注“已截断”，调用仍正常转发。请求头中的 API Key、Authorization 和门户 Cookie 不会写入记录；用户自行写进对话的秘密仍属于对话文本。未经网关的旧请求或直连 CPA 的请求没有可下载交互记录。更换 `PORTAL_APP_SECRET_FILE` 会使已有加密记录无法解密。

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
| `CPA_UPSTREAM_URL` | 空 | 网关访问 CPA 的内部地址；启用网关时必填 |
| `PORTAL_GATEWAY_LISTEN_ADDR` | 空（关闭） | 网关监听地址，与门户页面监听地址分开 |
| `PORTAL_GATEWAY_CAPTURE_DIR` | `/data/gateway-captures` | 加密交互记录目录 |
| `PORTAL_HOST_LOG_METRICS_PATH` | `/data/host-log-metrics.json` | 宿主机日志大小摘要；缺失或超过 5 分钟未更新时显示待采集 |
| `CPAMP_BASE_URL` | `http://host.docker.internal:18317` | CPAMP Manager Server 地址 |
| `CPAMP_ADMIN_KEY_FILE` | `/run/secrets/cpamp_admin_key` | CPAMP 管理 Key 文件 |
| `PORTAL_APP_SECRET_FILE` | `/run/secrets/portal_app_secret` | 至少 32 字符的会话/CSRF 密钥文件 |
| `PORTAL_RECONCILE_INTERVAL` | `5m` | Key 对账间隔 |
| `PORTAL_PENDING_RETRY` | `30s` | 待撤销任务重试间隔 |
| `PORTAL_USAGE_CACHE_TTL` | `2m` | CPAMP 用量查询的进程内短缓存；重启即清空 |
| `PORTAL_REGISTRATION_OPEN` | `true` | 仅首次建库时的注册开关默认值 |
| `PORTAL_TRUST_PROXY_HEADERS` | `false` | 是否信任 `X-Forwarded-For`；直连公网时保持 false |

### 宿主机日志占用采集

系统管理页直接统计门户 SQLite、CPAMP SQLite 和加密交互记录；CPA 主日志、CPA 请求/响应日志及三个容器的运行日志由宿主机每分钟生成一次大小摘要。门户只读取数字，不读取日志内容，也不挂载 Docker socket。按实际目录/容器名调整 `ops/collect-host-log-metrics.py` 和 service 后，在宿主机安装定时器：

```bash
sudo cp ops/cliproxy-portal-log-metrics.service ops/cliproxy-portal-log-metrics.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cliproxy-portal-log-metrics.timer
sudo systemctl start cliproxy-portal-log-metrics.service
```

默认 service 适配 `/root/cliproxy-portal` 下的门户数据目录和 `/root/cpa-manager-plus/cliproxyapi/logs` 下的 CPA 日志；容器名在脚本顶部。摘要不含日志正文或文件路径，未采集、过期或不可读取时页面显示待采集，不显示旧数值。

## 本地验证

```bash
go test ./...
go vet ./...
go build ./cmd/portal
```

运行时资源上限在 `compose.yaml` 中设为 0.75 CPU、256 MiB 内存。Argon2id 登录/改密会短时使用较多内存，这是有意的密码保护成本。
