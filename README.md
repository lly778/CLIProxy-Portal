# CLIProxy Portal

为 CLIProxyAPI（CPA）和 CPA-Manager-Plus（CPAMP）提供多用户管理门户，集中处理账号审批、API Key、模型配置、用量查询和系统维护。

项目使用 Go、服务端渲染页面和 SQLite WAL，前端资源嵌入单个可执行文件。运行时无需 Node.js、Redis 或独立数据库服务；可通过可选网关代理模型请求，也可通过 ACP 桥接接入外部 Antigravity CLI。

## 主要功能

| 功能 | 说明 |
| --- | --- |
| 账号与权限 | 手机号注册、人工审批、角色管理、停用与恢复、密码重置 |
| API Key | 用户自助领取与撤销，密码确认，每位批准用户最多一个有效 Key |
| 用量与日志 | 个人及全局请求、Token、成功率、模型排行、趋势与请求日志 |
| 上游管理 | OAuth 账号、额度、模型启停、别名、思考强度上限和跨渠道调用预设 |
| 模型网关 | 转发模型请求，提供加密交互记录、分段耗时和资源准入保护 |
| Antigravity ACP | 通过 agy/ 模型接入外部 CLI，支持客户端工具回传、图片、长对话和中断续接 |
| Codex 配置同步 | 下载单个批处理脚本，同步管理员选定的模型、默认模型和思考强度 |
| 资源监测 | CPU、内存、并发、历史趋势、自动暂停与恢复，以及可选 ESXi 宿主机监测 |
| 备份与恢复 | 加密备份到 GitHub 私有仓库 Releases，支持门户恢复及配套上游重建 |

页面适配桌面与移动端。上游账号额度由用户共享，门户中的额度展示不等于个人配额。

## 架构与职责

| 组件 | 职责 |
| --- | --- |
| CLIProxy Portal | 用户、权限、Key 生命周期、配置界面、统计与可选请求网关 |
| CPA | 常规模型请求、凭证调度和实际生成 |
| CPAMP | 向门户提供 CPA 管理接口与用量数据 |
| Antigravity CLI（可选） | 通过门户内置 ACP/MCP 桥接提供独立的 agy/ 模型通道 |

不开启网关时，模型客户端直接请求 CPA，门户通过 CPAMP 管理配置和查询用量。开启网关后，客户端请求先经过门户，再转发到 CPA 或已配置的 AGY 通道。网关侧的交互记录、请求耗时和资源保护要求流量经过门户。

CPA 渠道的新增授权在 CPAMP 完成；AGY 的登录与账号切换在门户管理界面完成。文件、终端和电脑操作由客户端提供的执行器处理，门户不会因此获得客户端项目文件。

## 快速开始

### 环境要求

- 可用的 CPA 和 CPAMP Full/Manager Server，以及 CPAMP 管理员 Key。
- Linux、Docker Engine 和 Docker Compose。默认 Compose 使用 systemd slice 与 cgroup v2；其他环境需调整相关配置。
- 在本机编译时需要 Go 1.24 或更高版本；也可以使用 Docker 源码构建。
- 门户能够访问 CPAMP；启用网关时，还需能够访问 CPA 内部接口。

以下以 Linux x86_64 和预编译方式为例。命令在项目根目录执行，地址、网络和凭据由安装者配置。

### 1. 获取源码并编译

```bash
git clone https://github.com/lly778/CLIProxy-Portal.git
cd CLIProxy-Portal
cp .env.example .env
mkdir -p dist data secrets
chmod 700 secrets

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o dist/portal-linux-amd64 ./cmd/portal
```

ARM64 将 `GOARCH` 改为 `arm64`，输出为 `dist/portal-linux-arm64`，并在 `.env` 设置 `PORTAL_BINARY=portal-linux-arm64`。

### 2. 配置地址与密钥

编辑 `.env`，区分对外地址和服务间地址：

| 配置 | 用途 | 示例 |
| --- | --- | --- |
| `PORTAL_EXTERNAL_URL` | 浏览器访问门户的地址 | `https://portal.example.com` |
| `CPA_API_BASE_URL` | 展示给客户端的模型 API 地址，不带末尾 /v1 | `https://api.example.com` |
| `CPAMP_BASE_URL` | 门户访问 CPAMP 的内部地址 | `http://host.docker.internal:18317` |
| `CPA_UPSTREAM_URL` | 网关访问 CPA 的内部地址，启用网关时使用 | `http://cli-proxy-api:8317` |

容器内的 `localhost` 指向容器本身。使用 `host.docker.internal` 或跨 Docker 网络的服务名时，需要确认宿主机端口、服务名和网络可达。

全新安装生成应用密钥，并保存 CPAMP 管理员 Key：

```bash
openssl rand -hex 32 > secrets/portal_app_secret
read -r -s -p "CPAMP 管理员 Key：" portal_cpamp_key
printf '\n'
printf '%s' "$portal_cpamp_key" > secrets/cpamp_admin_key
unset portal_cpamp_key

chmod 600 secrets/portal_app_secret secrets/cpamp_admin_key
sudo chown 10001:10001 data secrets/portal_app_secret secrets/cpamp_admin_key
```

已有安装应保留原应用密钥，它用于会话安全和交互记录加密。CPAMP 管理员 Key 与用户调用模型的 API Key 用途不同；真实配置、密钥和数据库不应提交到仓库。公网访问使用 HTTPS。

### 3. 准备资源组并启动

默认 Compose 引用了 `cliproxy.slice`，可按仓库模板创建：

```bash
sudo install -m 644 ops/cliproxy.slice /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cliproxy.slice

docker compose -f compose.prebuilt.yaml up -d --build
docker compose -f compose.prebuilt.yaml ps
curl -fsS http://127.0.0.1:18080/healthz
```

预编译方式只构建运行环境镜像，挂载 `dist/` 中的程序，数据保存在项目的 `data/`。也可使用 `docker compose -f compose.yaml up -d --build` 在 Docker 内编译源码，其数据使用命名卷 `portal-data`。两种方式运行同一个门户服务，切换前需要处理不同的数据挂载。

两种运行镜像均基于 Debian，包含 CA 证书和时区数据，以 UID/GID `10001:10001` 运行。资源组配置及其他环境的调整见 [资源保护与监测](ops/resources/README.md)。备份后台和重建恢复还需按 [备份文档](ops/backup/README.md) 初始化宿主机任务。

### 4. 创建首位管理员

```bash
read -r -s -p "初始管理员密码：" portal_initial_password
printf '\n'
printf '%s' "$portal_initial_password" > secrets/initial_admin_password
unset portal_initial_password
chmod 600 secrets/initial_admin_password
sudo chown 10001:10001 secrets/initial_admin_password

docker compose -f compose.prebuilt.yaml run --rm \
  -v "$PWD/secrets/initial_admin_password:/run/secrets/initial_admin_password:ro" \
  cliproxy-portal create-admin \
  --phone 13800138000 --name 系统管理员 \
  --password-file /run/secrets/initial_admin_password

rm -- secrets/initial_admin_password
```

替换示例手机号和姓名。管理员密码至少 14 个字符，普通用户密码至少 10 个字符，均不能包含完整手机号。创建成功后访问 `/login`；使用其他 Compose 方式时，上述命令需沿用启动服务的文件组合。

## 配置与使用

### 用户和 API Key

用户注册后由管理员审批。获准用户可以在账号页面领取或撤销 Key，操作需要验证登录密码。完整 Key 仅在领取时展示，门户数据库保存哈希与末四位。

门户保留上游已有手工 Key，不自动绑定到用户；上游手动删除的 Key 不会自动补回。Key 对账和撤销重试由后台处理，上游不可用时不会将未完成的撤销标记为成功。

### 模型、预设与客户端同步

管理员在上游管理中配置模型启停、别名、保留原名和思考强度上限，也可以保存、排序和应用跨渠道预设。预设只保存模型配置，不包含 OAuth 凭证。模型与档位以对应上游的声明为准。

配置客户端可选模型和默认值后，用户可下载 `Sync-Codex.bat` 同步本机 Codex 配置。脚本保留其他配置，验证后应用并生成本地回退副本；API Key 由用户提供或沿用本机已有配置。配置细节见 [Codex 同步说明](internal/clientsync/README.md)。

### 可选模型网关

网关通过 `PORTAL_GATEWAY_LISTEN_ADDR` 启用，`CPA_UPSTREAM_URL` 指向 CPA 内部接口，`CPA_API_BASE_URL` 指向提供给客户端的入口。

仓库提供 [compose.gateway.yaml](compose.gateway.yaml) 作为覆盖示例：默认加入 `cpamp_default` 网络，访问 `cli-proxy-api:8317`，将网关端口发布为宿主机 `8317`。使用前需按实际网络调整，并处理与 CPA 现有端口映射的冲突。

```bash
docker compose -f compose.prebuilt.yaml -f compose.gateway.yaml up -d --build
```

网关可记录请求分段耗时、加密保存交互文本，并按权限提供下载。交互记录与 AGY 的完整会话恢复索引是不同数据；系统指令、推理和图片等内容不作为普通文本交互记录长期保存。上游实际 Token 与门户传输结果分开统计，未完整交付的请求仍可能已经消耗上游额度。

部分非 Codex 渠道提供可选的 Codex 身份指令兼容开关，默认关闭，仅作用于经过网关且匹配规则的指令介绍，不修改用户消息或工具结果。

### 可选 Antigravity ACP

AGY 通道需要单独安装 Antigravity CLI、配置持久化目录和内部服务密钥，并完成 Google 授权。通过 `agy/` 前缀区分模型，沿用门户账号审批、Key 状态和模型启停设置。

该通道支持 Responses 流式与非流式请求、客户端函数与自定义工具、图片回传、工具目录更新、长历史摘要及受限中断恢复。工具结果按原调用核对，未知操作不会被默认当作成功或自动重复。具体协议、限制和配置见 [AGY 接入说明](ops/agy/README.md)。

### 资源保护与备份

资源保护支持观察和保护模式。保护模式依据并发、CPU 和内存控制新请求准入，压力解除后自动恢复；正在执行的请求继续处理。可选 ESXi 采集器用于物理宿主机监测。详见 [资源文档](ops/resources/README.md)。

备份使用独立恢复密钥加密并上传到 GitHub 私有仓库 Releases，可恢复门户数据，或按配置重建 CPA/CPAMP 并恢复配套数据。恢复密钥应独立保管；AGY 登录凭据可以纳入备份，会话与 CLI 程序不纳入。范围、宿主机初始化和恢复步骤见 [备份文档](ops/backup/README.md)。

## 常用配置

以下为程序配置；默认值与完整读取逻辑见 [config.go](internal/config/config.go)。`.env` 只用于替换 Compose 中显式引用的变量，其他配置需加入 Compose 的 `environment`。

| 变量 | 用途 |
| --- | --- |
| `PORTAL_LISTEN_ADDR` | 门户监听地址，默认 :18080 |
| `PORTAL_DATABASE_PATH` | 门户 SQLite 路径，默认 /data/portal.db |
| `PORTAL_EXTERNAL_URL` | 门户对外地址 |
| `CPA_API_BASE_URL` | 客户端使用的模型 API 基础地址 |
| `CPAMP_BASE_URL` | CPAMP 管理接口地址 |
| `CPAMP_ADMIN_KEY_FILE` | CPAMP 管理员 Key 文件 |
| `PORTAL_APP_SECRET_FILE` | 门户应用密钥文件 |
| `PORTAL_GATEWAY_LISTEN_ADDR` | 可选模型网关监听地址，空值关闭 |
| `CPA_UPSTREAM_URL` | 网关使用的 CPA 内部地址 |
| `PORTAL_GATEWAY_CAPTURE_DIR` | 加密交互记录目录 |
| `PORTAL_USAGE_CACHE_TTL` | 用量查询缓存时长，默认 2m |
| `PORTAL_TIME_ZONE` | 页面时区，默认 Asia/Shanghai |
| `PORTAL_REGISTRATION_OPEN` | 首次建库时的注册开关默认值 |
| `PORTAL_TRUST_PROXY_HEADERS` | 是否信任受控反向代理的转发 IP 头 |
| `PORTAL_HOST_METRICS_PATH` | 可选宿主机监测数据文件 |
| `PORTAL_BACKUP_STATE_DIR` | 备份配置、凭据和任务状态目录 |

## 开发

```bash
go test ./...
go vet ./...
go build ./cmd/portal
node --test internal/webui/tests/*.test.cjs
python -B -m unittest discover -s ops -p 'test_optimize_cpamp_request_indexes.py'
```

前端测试使用 Node.js 内置测试运行器，不需要安装 npm 依赖。Python 用于部分运维工具与测试，不是门户运行时依赖。

| 目录 | 内容 |
| --- | --- |
| `cmd/portal/` | 服务入口、管理员初始化和备份 CLI |
| `internal/agy/` | ACP/MCP 桥接、工具调用、摘要和会话恢复 |
| `internal/backup/` | 快照、加密、上传、调度与恢复 |
| `internal/clientsync/` | Codex 配置同步脚本与下载生成 |
| `internal/cpamp/` | CPAMP 接口客户端 |
| `internal/gateway/` | 请求代理、交互记录与计时 |
| `internal/httpserver/` | HTTP 路由、权限和视图数据 |
| `internal/service/` | 账号、Key、额度和模型业务逻辑 |
| `internal/store/` | SQLite 存储与迁移 |
| `internal/webui/` | 页面模板、样式、脚本及前端测试 |
| `ops/` | 配置示例、采集器、诊断和维护工具 |

提交问题时，请提供相关组件版本、复现步骤和脱敏错误信息。贡献代码时附上变更说明及相关验证结果。

## 文档

- [Antigravity ACP 接入](ops/agy/README.md)
- [DSH 客户端配置](ops/agy/dsh.md)
- [Codex 配置同步](internal/clientsync/README.md)
- [资源保护与监测](ops/resources/README.md)
- [加密备份与恢复](ops/backup/README.md)
- [网络诊断工具](ops/portal-network-diagnostics.md)
