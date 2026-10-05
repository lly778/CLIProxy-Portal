# CLIProxy 账号门户

为 CLIProxyAPI（CPA）与 CPA-Manager-Plus（CPAMP）提供账号注册、人工审批、API Key 自助管理和用量查询。

门户使用 Go、服务端渲染页面与 SQLite WAL，前端资源嵌入单个可执行文件。运行时不需要 Node.js、Redis 或独立数据库。模型请求仍由 CPA 鉴权和调度；门户不是独立模型服务。

## 功能

- **账号与审批**：手机号注册、人工批准或拒绝、拒绝后重提；管理员可停用、恢复、修改手机号、调整角色及删除账号。
- **API Key**：每位批准用户最多一个有效 Key；领取与撤销需确认登录密码。完整 Key 仅领取时显示，门户数据库保存哈希和末四位，不保存完整 Key。
- **用量与日志**：用户及全局用量、请求数与 Token 趋势、实际模型排名、请求日志及操作审计。
- **上游管理**：按 OAuth 渠道启停凭证和模型、维护模型别名、设置支持渠道的思考强度上限。
- **调用预设**：统一保存、应用和删除所有 OAuth 渠道的模型、别名及支持渠道的思考上限配置；共用最多 20 个，同名保存更新已有预设，不包含 OAuth 凭证。旧预设在数据库迁移时升级为新版多渠道格式，同名渠道预设合并；未记录的渠道在应用时保持当前配置。
- **共享额度池**：Codex 与 Antigravity 分别展示上游账号及模型组额度；这些是所有用户共享的上游资源，不是个人限额。
- **可选模型网关**：提供模型目录过滤、加密交互记录下载和服务器总耗时统计。
- **运维管理**：注册开关、版本化使用规则、Key 对账、系统健康与存储占用检查。

页面适配桌面与移动端。共享额度池支持拖动、滑动、箭头和键盘切换；请求日志与操作日志在空间不足时横向滚动。调用预设保存区宽屏并排、中宽屏右侧上下排列、窄屏全宽上下排列。

### 使用边界

- 新的上游 OAuth 凭证仍在 CPAMP 添加；门户管理已有凭证及其配置。
- 额度查询和思考强度上限目前针对 Codex、Antigravity 接入；其他渠道以 CPAMP 实际提供的能力为准，不套用 Codex 数据。
- 别名共享实际模型的思考上限。规则只降低明确超过上限的请求，不提高较低值，也不改未指定或自动预算。
- 停用模型会移除该模型的别名及思考上限；配置变更影响所选渠道的后续请求。
- 保存别名、恢复原名、启用模型及应用预设时，会检查跨 OAuth 渠道的别名冲突（不区分大小写），包括别名与另一渠道可见原名重名；共享的模型原名不受影响。无法读取其他渠道配置时会阻止变更。此检查仅约束门户内的操作，不拦截 CPAMP/CPA 外部直接修改。
- 门户不把既有手工 Key 导入用户账号；修改 Key 列表时会保留它们。避免门户与 CPAMP 同时修改同一份配置。
- CPAMP 不可用时不会误报撤销成功：待处理撤销任务默认每 30 秒重试，Key 默认每 5 分钟对账。CPAMP 中手动删除的 Key 不会自动补回。

## 部署前提

- Go 1.24 或更高版本，用于编译；运行已编译文件时不需要 Go。
- Linux 主机安装 Docker Engine 与 Docker Compose。
- 已有可用的 CPA 和 CPAMP **Full/Manager Server**；CPAMP 能访问 CPA 管理接口。
- 准备 CPAMP 管理员 Key，不能用普通客户端 API Key 替代。
- 确认门户到 CPAMP、网关到 CPA 的网络连通性及端口分配。

公网入口应通过反向代理提供 HTTPS。示例中的 HTTP 内部地址仅用于本机或可信网络；密码哈希、CSRF 等应用层措施不能保护明文 HTTP 链路。

### 地址的区别

| 配置 | 用途 | 示例 |
|---|---|---|
| `PORTAL_EXTERNAL_URL` | 用户访问门户的地址 | `https://portal.example.com` |
| `CPA_API_BASE_URL` | 展示给用户的模型 API 基础地址，不带末尾 `/v1` | `https://api.example.com` |
| `CPAMP_BASE_URL` | 门户访问 CPAMP 的内部地址 | `http://host.docker.internal:18317` |
| `CPA_UPSTREAM_URL` | 可选网关访问 CPA 的内部地址 | `http://cli-proxy-api:8317` |

容器中的 `localhost` 指容器自身。CPAMP 在其他 Docker 网络中时，需要加入对应网络并使用可解析的容器名；`host.docker.internal` 则依赖宿主机端口确实可从容器访问。

## 快速部署

以下命令在 Linux Bash 中运行。先准备配置与秘密文件，再选择一种启动方式。

### 1. 准备配置与秘密文件

```bash
cp .env.example .env
mkdir -p secrets
chmod 700 secrets
openssl rand -hex 32 > secrets/portal_app_secret
read -r -s -p 'CPAMP 管理员 Key：' cpamp_admin_key
printf '\n'
printf '%s' "$cpamp_admin_key" > secrets/cpamp_admin_key
unset cpamp_admin_key
chmod 600 secrets/portal_app_secret secrets/cpamp_admin_key
sudo chown 10001:10001 secrets/portal_app_secret secrets/cpamp_admin_key
```

编辑 `.env` 中的三个基础地址。不要把 `.env`、管理员 Key、应用密钥或数据库提交到 Git。已有部署必须保留原 `portal_app_secret`，不要重新生成：更换它会使已有加密交互记录无法解密。

### 2. 选择启动方式

两种方式的容器名相同，但数据挂载不同。不要同时启动，也不要直接切换而不迁移数据。

| 方式 | Compose 文件 | 二进制来源 | 数据存储 |
|---|---|---|---|
| 预编译部署 | `compose.prebuilt.yaml` | 宿主机 `dist/portal-linux-amd64` | 项目下 `./data` |
| 源码构建部署 | `compose.yaml` | Docker 多阶段构建 | Compose 命名卷 `portal-data` |

#### 方式 A：预编译部署

适合低内存 VPS。在开发机或构建机编译：

```bash
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o dist/portal-linux-amd64 ./cmd/portal
```

Windows PowerShell 编译同一 Linux 二进制：

```powershell
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags='-s -w' -o dist/portal-linux-amd64 ./cmd/portal
```

编译结果就在指定的 `dist/portal-linux-amd64`，该目录不纳入 Git。ARM64 主机改为 `GOARCH=arm64`，输出 `portal-linux-arm64`，并在 `.env` 设置 `PORTAL_BINARY=portal-linux-arm64`。

将二进制、Compose 文件、`.env` 和秘密文件放到服务器项目目录，然后启动：

```bash
chmod 755 dist/portal-linux-amd64
mkdir -p data
sudo chown 10001:10001 data
docker compose -f compose.prebuilt.yaml up -d
docker compose -f compose.prebuilt.yaml ps
curl -fsS http://127.0.0.1:18080/healthz
```

此方式不在 VPS 编译，只拉取 Alpine 运行镜像。若内部上游使用 HTTPS，应确认运行镜像信任所需 CA，必要时使用包含相应证书的自定义镜像，不要关闭证书校验。

#### 方式 B：Docker 源码构建

```bash
docker compose -f compose.yaml up -d --build
docker compose -f compose.yaml ps
curl -fsS http://127.0.0.1:18080/healthz
```

Dockerfile 使用 Go 多阶段构建，并在运行镜像中安装 CA 证书和时区数据。两种 Compose 当前均限制为 0.75 CPU、192 MiB 内存；密码哈希操作会短时使用较多内存。

### 3. 创建首位管理员

普通用户密码至少 10 个字符，管理员至少 14 个字符，均不能包含完整手机号。

以下示例使用预编译部署；源码构建部署把 `-f compose.prebuilt.yaml` 改为 `-f compose.yaml`。启用网关后，日常命令应保持与启动时相同的 Compose 文件组合。

```bash
read -r -s -p '初始管理员密码：' portal_admin_password
printf '\n'
printf '%s' "$portal_admin_password" > secrets/initial_admin_password
unset portal_admin_password
chmod 600 secrets/initial_admin_password
sudo chown 10001:10001 secrets/initial_admin_password
docker compose -f compose.prebuilt.yaml run --rm \
  -v "$PWD/secrets/initial_admin_password:/run/secrets/initial_admin_password:ro" \
  cliproxy-portal create-admin \
  --phone 13800138000 --name 系统管理员 \
  --password-file /run/secrets/initial_admin_password
rm -- secrets/initial_admin_password
```

替换示例手机号和姓名，确认创建成功后访问 `/login`。管理员可在“使用规则”中调整规则与注册开关，在“用户管理”中审批账号；密码重置使用管理员签发的 15 分钟一次性重置码。

## 可选模型网关

不开启网关时，用户直接调用 CPA，门户仍可查询 CPAMP 提供的用量与日志。开启网关后，客户端请求必须经过网关才能生成门户交互记录和总耗时样本。

手动配置至少需要：

```dotenv
PORTAL_GATEWAY_LISTEN_ADDR=:18318
CPA_UPSTREAM_URL=http://cli-proxy-api:8317
```

还需映射网关端口、连通 CPA，并将 `CPA_API_BASE_URL` 指向网关的公网入口。不要把 `CPA_UPSTREAM_URL` 指回网关，否则会形成循环。

仓库的 `compose.gateway.yaml` 是特定网络拓扑的覆盖文件：

- 网关容器监听 `18318`，发布到宿主机 `8317`。
- CPA 地址固定为 `http://cli-proxy-api:8317`。
- 门户加入已有的 `cpamp_default` Docker 网络。

先确认该网络、容器名和端口有效，并迁移 CPA 原有的宿主机 `8317` 映射以避免冲突，再启动：

```bash
docker compose -f compose.prebuilt.yaml -f compose.gateway.yaml \
  up -d --no-deps cliproxy-portal
```

不同部署拓扑应调整覆盖文件；它不是通用的一键安装脚本。

### 模型目录与交互记录

- 网关过滤 `GET /v1/models` 中仅作为 Codex 别名的名称，别名调用仍由 CPA 路由；与真实已启用模型重名的名称保留。
- Antigravity 冷却后的目录缺项只依据 CPA 当前明确可调度的凭证和已注册模型恢复，不猜测模型、不重置冷却，也不发起生成请求。
- 支持 Responses、Chat Completions、Messages 和 Gemini 的用户/助手文本及实际工具调用、参数、结果；不保存系统/开发者指令、推理、工具定义、图片等非文本内容。
- 记录加密存储，同一用户的重复事件去重；用户只可下载自己的记录，管理员可下载全局记录。默认 JSON，下载链接增加 `?format=txt` 可获取文本。
- 每位用户的交互记录及计时明细，按有、无 CPA 请求 ID 分组，各保留最近 100 条；长期统计样本不受该明细上限影响，也不会按天自动清除。
- 请求或响应的记录副本单侧超过 16 MB 时标注截断，不截断实际代理流量。旧格式交互记录会在启动清理时移除。
- Authorization、API Key 请求头及门户 Cookie 不写入交互记录；用户写进对话正文的秘密仍可能被保存。

网关不额外设置对话读取、写入或连接空闲超时。记录解析与持久化在响应结束后异步完成；代理仍遵循 HTTP 转发语义，不是所有协议字段的逐字节透传。

### 统计口径

| 指标 | 来源与边界 |
|---|---|
| 请求数、Token、成功率/失败率 | CPAMP 的用量数据；全局统计可能包含未绑定用户的手工 Key |
| 按模型排行 | 优先 `resolved_model`，其次 `response_model`；都缺失则记为“未记录实际模型” |
| 请求日志“耗时” | 门户网关记录的服务器总耗时，不用 CPA 模型延迟替代 |
| 平均耗时与请求健康趋势 | 实际网关计时样本，包括失败及中断请求，不包含未计时的直连请求 |

最近 7 天使用精确的 168 小时范围，小时数据合并为每 3 小时一段。实际模型排名会分页读取所选区间的完整请求元数据并核对总量；明细缺失或核对失败时暂停排行，不拿最近 100 条日志代替历史数据。

总耗时分为接入及上传、等待回复、生成及发送三个阶段；它不包含服务器接入前的 DNS/TCP 握手、客户端准备和最终显示。计时通过完整 CPA 请求 ID 与 Key 哈希关联，不按时间近似匹配；历史或未关联请求显示“—”。模型调用与首 Token 指标可能与阶段计时重合，不能再累加。健康趋势仅在区段全部样本具有有效边界时展示三段堆叠。

## 配置参考

以下默认值来自 `internal/config/config.go`。Compose 会覆盖其中部分值，例如将 `CPAMP_BASE_URL` 设置为 `http://host.docker.internal:18317`。

| 变量 | 程序默认值 | 说明 |
|---|---|---|
| `PORTAL_LISTEN_ADDR` | `:18080` | 门户监听地址 |
| `PORTAL_DATABASE_PATH` | `/data/portal.db` | SQLite 文件 |
| `PORTAL_EXTERNAL_URL` | `http://localhost:18080` | 门户对外地址 |
| `CPA_API_BASE_URL` | `http://localhost:8317` | 展示给用户的模型 API 基础地址 |
| `CPAMP_BASE_URL` | `http://cpa-manager-plus:18317` | CPAMP Manager Server 地址 |
| `CPAMP_ADMIN_KEY_FILE` | `/run/secrets/cpamp_admin_key` | CPAMP 管理员 Key 文件 |
| `PORTAL_APP_SECRET_FILE` | `/run/secrets/portal_app_secret` | 至少 32 字符的应用密钥文件 |
| `PORTAL_COOKIE_NAME` | `cliproxy_portal_session` | 会话 Cookie 名称 |
| `PORTAL_TIME_ZONE` | `Asia/Shanghai` | 页面时间显示时区 |
| `CPAMP_TIMEOUT` | `15s` | CPAMP 管理请求超时 |
| `PORTAL_RECONCILE_INTERVAL` | `5m` | Key 对账间隔 |
| `PORTAL_PENDING_RETRY` | `30s` | 待处理撤销任务重试间隔 |
| `PORTAL_USAGE_CACHE_TTL` | `2m` | 用量查询进程内缓存，重启清空 |
| `PORTAL_REGISTRATION_OPEN` | `true` | 仅首次建库时的注册默认值 |
| `PORTAL_TRUST_PROXY_HEADERS` | `false` | 是否信任转发 IP 请求头，只在受控代理后按需启用 |
| `PORTAL_GATEWAY_LISTEN_ADDR` | 空，关闭网关 | 网关独立监听地址 |
| `CPA_UPSTREAM_URL` | 空 | 启用网关时必填 |
| `PORTAL_GATEWAY_CAPTURE_DIR` | `/data/gateway-captures` | 加密交互记录目录 |
| `PORTAL_HOST_LOG_METRICS_PATH` | `/data/host-log-metrics.json` | 宿主机日志大小摘要 |
| `PORTAL_BINARY` | `portal-linux-amd64`（Compose 默认） | 预编译 Compose 挂载的文件名，不是程序环境变量 |

`.env` 只为 Compose 中显式写出的变量提供替换值，不会自动把表中全部变量传入容器。调整其他配置时，修改 Compose 的 `environment` 或使用自己的覆盖文件，并重新创建容器。

## 日常运维

### 查看状态与日志

预编译部署：

```bash
docker compose -f compose.prebuilt.yaml ps
docker compose -f compose.prebuilt.yaml logs -f --tail=100 cliproxy-portal
curl -fsS http://127.0.0.1:18080/healthz
```

`/healthz` 返回 `ok` 表示门户 HTTP 服务可用，不代表 CPAMP、CPA 或模型额度全部正常；详细状态在系统管理页检查。源码构建部署改用 `compose.yaml`；启用网关时加上 `-f compose.gateway.yaml`。

### 更新版本

1. 在构建机测试并生成新的二进制，按服务器架构选择文件。
2. 保留当前二进制与一致性数据库备份，将新文件以临时文件名上传。
3. 在服务器项目目录替换二进制，再重新创建门户容器。以下假定已上传 `dist/portal-linux-amd64.new`：

```bash
chmod 755 dist/portal-linux-amd64.new
mv -- dist/portal-linux-amd64.new dist/portal-linux-amd64
docker compose -f compose.prebuilt.yaml up -d --no-deps --force-recreate cliproxy-portal
curl -fsS http://127.0.0.1:18080/healthz
```

网关部署使用相同的两个 Compose 文件。不要只执行 `restart`：文件级绑定挂载更换后，需要重建容器才能确保使用新文件。失败时恢复原二进制并重建；涉及数据库迁移的版本应另行评估数据回滚，不要盲目恢复旧数据库覆盖新数据。

源码构建部署使用 `docker compose -f compose.yaml up -d --build --no-deps cliproxy-portal`。

### 数据与备份

应用不自动创建部署备份。数据库使用 WAL：服务运行时不要只复制 `portal.db` 当作完整备份，应使用 SQLite 一致性备份，或停服后按实际状态备份数据目录。备份中同时保留应用密钥与加密记录，注意限制访问权限。

预编译部署的数据在项目的 `data/`；源码构建部署的数据在命名卷。删除数据目录、命名卷或执行 `docker compose down -v` 会丢失相应部署的数据。备份和诊断报告可能包含敏感信息，不要公开上传。

### 宿主机日志占用采集

系统管理页直接查询门户数据库、CPAMP 数据库及交互记录占用。CPA 主日志、请求/响应日志和容器日志大小由宿主机采集脚本生成摘要；缺失或超过 5 分钟未更新时显示待采集。

先检查 `ops/collect-host-log-metrics.py` 的容器名，以及 service 的脚本、日志和输出路径。当前 service 示例面向 `/root/cliproxy-portal` 与 `/root/cpa-manager-plus/cliproxyapi/logs`，不是所有安装都能直接使用。

```bash
sudo cp ops/cliproxy-portal-log-metrics.service ops/cliproxy-portal-log-metrics.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cliproxy-portal-log-metrics.timer
sudo systemctl start cliproxy-portal-log-metrics.service
```

门户只读取大小摘要，不读取这些日志正文，也不挂载 Docker socket。

## 开发与验证

```bash
go test ./...
go vet ./...
go build ./cmd/portal
node --test internal/webui/tests/*.test.cjs
```

前端测试使用 Node.js 内置测试运行器，不需要 `npm install` 或 `node_modules`。覆盖额度轮播、拖动与选字、导航滚动、渠道切换、刷新状态和图表手势；这些模拟测试不替代真实浏览器的视觉与交互验证。

Windows PowerShell 可先枚举测试路径：

```powershell
$portalTests = Get-ChildItem internal/webui/tests -Filter '*.test.cjs' |
  ForEach-Object { $_.FullName }
node --test $portalTests
```

项目位于 OneDrive 时，不要在同步目录中创建、安装或链接 `node_modules`。需要第三方 Node 依赖的处理应放在同步目录外的独立临时工作区，仅把最终产物复制回来。

### Windows 网络诊断

`ops/Run-PortalNetwork.cmd` 与 `ops/Test-PortalNetwork.ps1` 用于比较客户端网络中的 DNS、TCP、连接复用和请求上传时间。调用脚本时显式指定实际模型 API 地址：

```powershell
powershell -ExecutionPolicy Bypass -File ops/Test-PortalNetwork.ps1 -BaseUrl 'https://api.example.com/v1'
```

上传测试使用不存在的诊断模型，预期产生 400 日志，不调用真实模型；不要把它误认为业务请求故障。详情见[网络诊断说明](ops/portal-network-diagnostics.md)。

## 目录结构

```text
cmd/portal/         程序入口、管理员初始化命令
internal/config/   环境配置
internal/cpamp/    CPAMP 管理接口客户端
internal/service/  账号、Key、额度、模型与预设逻辑
internal/store/    SQLite 存储与迁移
internal/gateway/  模型代理、交互记录与分段计时
internal/httpserver/ 门户路由、权限与视图数据
internal/webui/    HTML 模板、静态资源与前端测试
ops/              宿主机日志采集与网络诊断工具
dist/             本地构建产物，不纳入 Git
```
