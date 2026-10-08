# CLIProxy Portal

为 CLIProxyAPI（CPA）和 CPA-Manager-Plus（CPAMP）提供用户账号、人工审批、API Key 自助管理、用量查询及上游配置管理。

门户由 Go 编写，使用服务端渲染页面和 SQLite WAL；HTML、CSS、JavaScript 嵌入单个可执行文件。运行时不需要 Node.js、Redis 或独立数据库服务。模型鉴权、凭证调度与实际生成仍由 CPA 完成。

## 功能概览

- **账号管理**：手机号注册、审批与拒绝后重提、停用与恢复、角色和手机号调整、管理员签发密码重置码。
- **API Key 管理**：每位批准用户最多一个有效 Key；领取、撤销需要验证登录密码。完整 Key 仅领取时展示，门户数据库保存哈希和末四位。
- **用量与交互记录**：个人及全局请求数、Token、成功率、模型排行和趋势；可选网关提供加密交互记录与分段耗时。
- **上游配置**：按 OAuth 渠道管理已有凭证、模型启用状态、别名和支持渠道的思考强度上限；Codex、Antigravity 展示共享额度池。
- **Codex 指令兼容**：非 Codex 渠道可通过模型卡片右上角的“兼容”开关，删除身份介绍中的 `based on GPT-5`，支持前缀、大小写和空白变体；默认关闭，仅对经过门户网关的请求生效。
- **调用预设与共享布局**：跨渠道预设最多 20 个，同名保存更新；预设和别名卡片采用瀑布流，可拖动或用键盘排序。布局保存在门户数据库，由所有管理员共享。
- **系统管理**：注册开关、版本化使用规则、Key 对账、操作审计、系统健康和存储占用检查。
- **加密备份与恢复**：手动或每周自动备份到 GitHub 私有仓库 Releases；支持仅恢复门户数据库，或重建 CPA/CPAMP 并恢复配套数据。

页面适配桌面与移动端。门户不创建新的上游 OAuth 凭证，新增授权仍在 CPAMP 完成。额度池是所有用户共享的上游资源，不是个人配额。

## 部署前提

- 已部署可用的 CPA 和 CPAMP Full/Manager Server，且 CPAMP 能访问 CPA 管理接口。
- 准备 **CPAMP 管理员 Key**，不能使用普通客户端 API Key 替代。
- Linux 服务器安装 Docker Engine 和 Docker Compose；源码编译需要 Go 1.24 或更高版本。
- 确认门户到 CPAMP、可选网关到 CPA 的网络连通性。
- 公网入口使用 HTTPS；管理接口不要直接暴露给不可信网络。

### 四类地址

| 配置 | 用途 | 示例 |
| --- | --- | --- |
| `PORTAL_EXTERNAL_URL` | 用户访问门户的地址 | `https://portal.example.com` |
| `CPA_API_BASE_URL` | 展示给用户的模型 API 基础地址，不带末尾 `/v1` | `https://api.example.com` |
| `CPAMP_BASE_URL` | 门户访问 CPAMP 的内部地址 | `http://host.docker.internal:18317` |
| `CPA_UPSTREAM_URL` | 可选网关访问 CPA 的内部地址 | `http://cli-proxy-api:8317` |

容器内的 `localhost` 指向容器本身。使用 `host.docker.internal` 时，宿主机端口必须可从容器访问；跨 Docker 网络部署时，需加入相应网络并使用可解析的服务名。

## 安装与启动

以下 Linux 命令在项目根目录执行。先准备配置和秘密文件，再选择一种部署方式。

### 1. 准备配置和秘密文件

```bash
cp .env.example .env
mkdir -p secrets
chmod 700 secrets

# 仅用于全新安装；已有部署必须沿用原文件。
openssl rand -hex 32 > secrets/portal_app_secret
read -r -s -p "CPAMP 管理员 Key：" portal_cpamp_key
printf "\n"
printf "%s" "$portal_cpamp_key" > secrets/cpamp_admin_key
unset portal_cpamp_key
chmod 600 secrets/portal_app_secret secrets/cpamp_admin_key
sudo chown 10001:10001 secrets/portal_app_secret secrets/cpamp_admin_key
```

编辑 `.env` 中的门户公网地址、模型 API 地址和 CPAMP 内部地址。不要将 `.env`、秘密文件、数据库或备份包提交到源码仓库。

`portal_app_secret` 用于门户会话安全和交互记录加密。已有部署不要重新生成，否则旧交互记录将无法解密。

### 2. 选择部署方式

| 方式 | Compose 文件 | 数据位置 | 适用场景 |
| --- | --- | --- | --- |
| 预编译部署 | `compose.prebuilt.yaml` | 项目下 `./data` | 在开发机编译，低内存 VPS 只运行 |
| Docker 源码构建 | `compose.yaml` | Compose 命名卷 `portal-data` | 在构建环境中生成运行镜像 |

两种方式使用相同容器名，不要同时启动。数据挂载不同，切换前必须迁移数据。

#### 预编译部署

在构建机生成 Linux x86_64 二进制：

```bash
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o dist/portal-linux-amd64 ./cmd/portal
```

Windows PowerShell 编译：

```powershell
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags="-s -w" -o dist/portal-linux-amd64 ./cmd/portal
```

ARM64 服务器将 `GOARCH` 改为 `arm64`，输出 `dist/portal-linux-arm64`，并在 `.env` 设置 `PORTAL_BINARY=portal-linux-arm64`。

将二进制、Compose 文件、`.env` 和秘密文件放到服务器项目目录，启动门户：

```bash
chmod 755 dist/portal-linux-amd64
mkdir -p data
sudo chown 10001:10001 data
docker compose -f compose.prebuilt.yaml up -d
docker compose -f compose.prebuilt.yaml ps
curl -fsS http://127.0.0.1:18080/healthz
```

默认运行镜像为 Alpine；内部上游使用 HTTPS 时，应确保镜像具备所需 CA 证书，必要时使用自定义运行镜像，不要关闭证书校验。

#### Docker 源码构建

```bash
docker compose -f compose.yaml up -d --build
docker compose -f compose.yaml ps
curl -fsS http://127.0.0.1:18080/healthz
```

Dockerfile 使用 Go 多阶段构建，运行镜像包含 CA 证书及时间区域数据。两种 Compose 均以 UID/GID `10001:10001` 运行，默认限制为 192 MiB 内存、0.75 CPU。

### 3. 创建首位管理员

以下示例使用预编译部署。源码构建部署改用 `-f compose.yaml`；启用网关后保持与启动时相同的 Compose 文件组合。

```bash
read -r -s -p "初始管理员密码：" portal_initial_password
printf "\n"
printf "%s" "$portal_initial_password" > secrets/initial_admin_password
unset portal_initial_password
chmod 600 secrets/initial_admin_password
sudo chown 10001:10001 secrets/initial_admin_password

docker compose -f compose.prebuilt.yaml run --rm \
  -v "$PWD/secrets/initial_admin_password:/run/secrets/initial_admin_password:ro" \
  cliproxy-portal create-admin \
  --phone 13800138000 --name 系统管理员 \
  --password-file /run/secrets/initial_admin_password

# 确认创建成功后删除临时密码文件。
rm -- secrets/initial_admin_password
```

替换示例手机号和姓名，然后访问 `/login`。普通用户密码至少 10 个字符，管理员至少 14 个字符，均不能包含完整手机号。管理员签发的密码重置码为 15 分钟有效的一次性凭据。

## 上游配置与 Key 对账

- 别名指向所选渠道的实际模型，可同时保留原名；别名与原名共用实际模型的思考上限。
- 思考上限只降低明确超出上限的请求，不提高较低值，也不改未指定或自动预算。
- 非 Codex 渠道可按渠道开启 [Codex 指令兼容](#codex-指令兼容)，作用于该渠道的模型及别名。
- 停用模型会移除对应别名和思考上限。预设保存模型配置，不包含 OAuth 凭证。
- 保存别名、启用模型、保留原名及应用预设时，会检查跨 OAuth 渠道的别名冲突，包括与其他渠道可见原名重名；不区分大小写。无法读取所需渠道配置时阻止变更。
- 上述检查仅约束门户操作，不能阻止 CPA/CPAMP 外部直接修改。避免多个管理端同时写入同一配置。
- 门户保留已有手工 Key，不将其自动绑定到用户。CPAMP 中手动删除的 Key 不会自动补回。
- 默认每 5 分钟对账，待处理撤销任务每 30 秒重试。CPAMP 故障时不会误报撤销成功。

### Codex 指令兼容

用于处理 Codex 的 GPT-5 身份描述在部分非 Codex 上游触发错误的情况。配置默认关闭，由管理员按渠道开启，对所有经过门户网关的该渠道请求生效。

1. 确认已启用 [模型网关](#可选模型网关)，且客户端 API 地址指向网关入口。
2. 在“上游管理”选择 Antigravity 等非 Codex 渠道，点击 **OAuth 模型卡片右上角的“兼容”开关**。开关与标题及其下方说明区域垂直居中，点击后直接保存，无需另点保存按钮。

保存结果在开关下方提示；保存失败时恢复上次保存的显示状态。Codex 渠道不提供此选项；未启用门户网关时开关不可用。设置持久化到门户数据库，开关即时生效并记录管理操作审计；随数据库备份及恢复保留，不随调用预设切换。

**匹配范围**：仅在指令开头的身份介绍句中删除 `based on GPT-5`，保留句号和其余内容。支持 `You are`、`You're`、`I am`、`I'm`、`This assistant is` 等介绍前缀，以及大小写、前导空白、连续空格、制表符和换行差异。

| 原始身份介绍 | 处理结果 |
| --- | --- |
| `You are Codex, a coding agent based on GPT-5.` | `You are Codex, a coding agent.` |
| `You are Codex, an assistant based on GPT-5.` | `You are Codex, an assistant.` |
| `I am an assistant BASED ON gpt-5.` | `I am an assistant.` |
| `You are an assistant based on GPT-5.1.` | 保留原样，其他型号不参与处理 |

| 接口 | 可处理的指令位置 |
| --- | --- |
| Responses | `instructions`，以及 `input` 中 `system` / `developer` 消息的文本 |
| Chat Completions | `messages` 中 `system` / `developer` 消息的文本 |
| Anthropic Messages | `system` 字符串或文本块 |
| Gemini 原生接口 | `systemInstruction.parts` 中的文本 |

用户消息、助手消息、工具结果、后续说明和引用示例不参与处理；GPT-5.1、GPT-5-mini 等其他型号保留原样。网关根据当前模型启用状态和别名配置判断渠道，名称包含 Codex 路由时保留原指令；多个非 Codex 渠道共用同一名称时，只有所有候选渠道均开启此选项才处理。

未知路由、渠道查询失败、原始或解压后的请求体超过 2 MiB、不支持的压缩格式及无法解码的请求均原样转发。该开关只处理身份描述兼容问题，不能保证消除所有上游 429；直连 CPA 的请求不受影响。

## 可选模型网关

不开启网关时，客户端直接调用 CPA，门户仍可查询 CPAMP 提供的用量数据。要启用 Codex 指令兼容，或生成门户交互记录与总耗时样本，客户端请求必须经过网关。

网关至少需要以下配置，并映射监听端口：

```dotenv
PORTAL_GATEWAY_LISTEN_ADDR=:18318
CPA_UPSTREAM_URL=http://cli-proxy-api:8317
```

将 `CPA_API_BASE_URL` 指向网关公网入口；`CPA_UPSTREAM_URL` 必须指向 CPA，不能指回网关。

仓库提供的 `compose.gateway.yaml` 面向特定拓扑：加入已有的 `cpamp_default` 网络，访问 `cli-proxy-api:8317`，将网关 `18318` 发布为宿主机 `8317`。先确认网络、服务名和端口，并处理 CPA 原有 `8317` 映射的冲突。

```bash
docker compose -f compose.prebuilt.yaml -f compose.gateway.yaml \
  up -d --no-deps cliproxy-portal
```

其他拓扑需调整覆盖文件；该文件不是 CPA/CPAMP 的通用安装脚本。

### 模型目录与交互记录

- 门户模型目录与 `GET /v1/models` 动态发现凭证及配置中的 OAuth 渠道，隐藏仅作为别名的名称，不限于 Codex；与任一渠道真实已启用模型重名的名称保留。渠道、别名或必要的模型启用状态读取失败时，不返回未过滤列表。别名调用仍由 CPA 路由，不改写请求模型名称。
- Antigravity 目录缺项仅依据 CPA 明确可调度的凭证和已注册模型恢复，不猜测模型、不重置冷却、不发起生成请求。
- 支持 Responses、Chat Completions、Messages 和 Gemini 的用户/助手文本，以及实际工具调用、参数和结果；不持久化系统/开发者指令、推理、工具定义或图片等非文本内容。
- 正文加密存储，同一用户的重复事件去重。用户只可下载自己的记录，管理员可下载全局记录；默认 JSON，增加 `?format=txt` 可下载文本。
- 每位用户的交互记录及计时明细，按有、无 CPA 请求 ID 分组，各保留最近 100 条。长期统计样本不受该明细上限影响。
- 单侧记录副本超过 16 MiB 会标注截断，不截断实际代理流量。原始报文只是临时解析输入，旧格式记录在启动清理时移除。
- 请求头中的 Authorization、API Key 和门户 Cookie 不写入记录；用户自行写入正文的秘密仍可能被保存。

网关不额外设置对话读写或连接空闲超时。记录解析和持久化在响应结束后异步完成；代理仍遵循 HTTP 转发语义，并非所有字段的逐字节透传。

### 统计口径

| 指标 | 数据来源与边界 |
| --- | --- |
| 请求数、Token、成功率 | CPAMP 用量数据；全局统计可能包含未绑定用户的手工 Key |
| 模型排行 | 优先 `resolved_model`，其次 `response_model`；均缺失时记为未记录实际模型 |
| 请求日志耗时、平均耗时 | 门户网关的服务器总耗时，不以 CPA 模型延迟替代 |
| 请求健康与分段趋势 | 实际网关计时样本，包含失败及中断请求，不包含未计时的直连请求 |

最近 7 天使用精确的 168 小时范围，每 3 小时聚合一段。模型排行分页读取所选区间的请求元数据并核对总量；核对失败时暂停排行，不用最近 100 条日志代替历史数据。

总耗时分为接入及上传、等待回复、生成及发送，不包含服务器接入前的 DNS/TCP 握手、客户端准备与最终显示。通过完整 CPA 请求 ID 和 Key 哈希关联，未关联记录显示“—”。模型调用、首 Token 指标可能与阶段计时重合，不能再次累加；区段全部样本边界有效时才显示三段堆叠。

## 备份与恢复

备份由宿主机 runner 执行，门户只负责配置与提交请求，不需要挂载 Docker socket 或上游目录。**仅启动门户容器不会安装定时备份任务**；先按 [备份与恢复部署说明](ops/backup/README.md) 配置来源、恢复策略和 systemd 任务。

### 备份设置

在“系统管理 → 存储占用 → 备份”展开设置，可填写专用 GitHub 私有仓库、访问令牌、每周自动备份开关、星期及北京时间（时:分），并下载独立恢复密钥。

- 首次设置的计划默认每周一北京时间 `03:00`，默认保留最近 3 份；启用并保存后生效。
- 已配置的令牌留空保持不变，不回显；下载密钥不会轮换已有密钥。
- 设置保存、密钥下载、手动备份和恢复均验证管理员密码及 CSRF，相关操作写入审计，秘密内容不进入日志。
- 保留份数、备份来源和批准的部署策略由后台配置，不能从网页选择任意服务器路径。
- 宿主机每分钟检查排队任务和调度，并非每分钟备份。本周计划已过且尚未尝试时补执行一次；自动失败不在本周循环重试，可手动重新请求。

加密包作为 `backup.cbackup` 上传到私有仓库 **Releases**，不提交到源码分支。流程包括一致性快照、压缩、加密、本地恢复验证、上传与远端下载校验；验证成功后才清理超出保留数量的旧成功备份。压缩率取决于数据内容，不能按数据库原大小保证固定比例。

低配置主机采用快速 gzip 压缩、复用加密缓冲区及流式解密验证，减少 CPU、内存分配和中间文件读写；快速压缩可能增加上传体积。同一份未变化的数据库快照不重复执行完整性检查，但生成快照和恢复后的数据库均须通过校验。宿主机 `status.json` 和任务日志记录各阶段及总耗时，便于区分快照、压缩、验证和网络传输的瓶颈。

### 低配置主机性能

当前采用 `VACUUM INTO` 一致性快照、gzip 快速等级 1、分块 AES-256-GCM 加密及流式恢复验证。快照仍会压实数据库，不使用实验性的在线备份 API。优化不减少备份范围，不跳过必要的完整性、认证或远端下载校验。

2026-10-06 在同一台主机上，以同一份约 639 MiB 的完整数据、50% CPU 配额和 384 MiB 内存上限进行单次对比：

| 指标 | 优化前 | 当前方案 | 变化 |
| --- | --- | --- | --- |
| 完整数据纯压缩耗时 | 16.9 秒 | 7.8 秒 | 减少约 54% |
| 本地全流程耗时 | 308.4 秒 | 176.8 秒 | 减少约 42.7% |
| 本地全流程 CPU 时间 | 81.0 秒 | 46.4 秒 | 减少约 42.7% |
| 加密备份包大小 | 92.3 MiB | 109.2 MiB | 增加约 18.3% |

本地全流程包含快照、压缩、加密、本地恢复验证及加密包 SHA256 读取；不包含测试数据准备、服务停启和 GitHub 上传下载。纯压缩是单独测量，不应再叠加到全流程耗时。CPU 时间表示处理器累计工作时间，耗时表示实际等待时间；结果受数据内容、磁盘、主机负载和网络影响，不保证每次备份都有相同比例的收益。

### 备份范围

| 内容 | 保留的信息 |
| --- | --- |
| 门户 `portal.db` | 用户及密码哈希、Key 元数据、使用规则、调用预设、渠道兼容开关、共享布局、审计和门户计时数据 |
| CPA `config.yaml`、`auths/` | 原始 API Key、模型别名、思考上限、启用状态及上游授权文件 |
| CPAMP `usage.sqlite`、`data.key` | 数据库内在线请求/用量统计、设置及其配套解密密钥 |
| 部署参数与秘密文件 | 配置来源中列出的 Compose、`.env`、管理密钥、门户应用密钥；清单记录上游固定镜像摘要 |

SQLite 通过 `VACUUM INTO` 生成独立一致性快照，包含已提交的 WAL 数据，不另行打包在线 `-wal`、`-shm`。CPAMP 在取快照期间短暂停止，完成后立即启动并检查健康，再进行压缩与上传；门户和 CPA 不因备份暂停。

当前自动备份不包含 `usage-archives/`、门户独立交互正文、普通日志、镜像文件、GitHub 令牌或备份恢复密钥。`data.key` 必须与 CPAMP 数据库成套保留。用户登录密码以数据库中的密码哈希随快照保留，不备份明文密码。

### 两种在线恢复

| 操作 | 覆盖范围 | 服务处理 |
| --- | --- | --- |
| 恢复 | 仅门户数据库 | 停止门户、替换数据库、启动并检查；不改 CPA/CPAMP 配置 |
| 重建并恢复 | 门户数据库、CPA 配置/auths、CPAMP 数据库/data.key，以及对应上游部署参数和管理密钥 | 下载备份指定镜像摘要，重建 CPA/CPAMP，再启动门户并检查 |

恢复时选择模式、验证管理员密码、确认覆盖并上传加密包。正常使用已有恢复密钥；服务器未配置密钥时，可在弹窗提供另行保管的密钥。校验失败或未确认不会覆盖。

普通恢复不会把上游 API Key、模型配置或启用状态还原到备份时间；重建并恢复会。两种模式均清空旧登录会话、密码重置码和待执行同步任务，完成后需要重新登录。在线恢复不会轮换当前门户应用密钥，也不会覆盖门户自身的 Compose 和 `.env`。

覆盖前会校验备份、检查磁盘余量并生成回退副本，失败尝试回退。不要手动删除未完成恢复事务、运行锁或回退目录。数据库若含未配套归档文件的 CPAMP 归档索引，自动重建恢复会拒绝该包；普通门户恢复不受影响。旧包缺少重建必需文件或镜像摘要时不能自动重建。

**全新服务器必须先准备门户程序、Docker/Compose、网络和后台策略**，不能仅凭恢复按钮从零建立整台主机。原生 `backup restore` 命令可校验解密到新目录，不覆盖文件、不启动服务；后续初始化及重建步骤见 [专项文档](ops/backup/README.md#全新服务器首次准备)。域名、隧道和防火墙需另行配置。

## 数据目录与密钥

预编译部署使用项目下 `data/`；源码构建部署使用命名卷中的 `/data`。

```text
data/
├── portal.db              # 门户数据库
├── portal.db-wal          # 在线 WAL，可能有尚未合并到主文件的已提交数据
├── portal.db-shm          # SQLite 共享内存索引
├── backup/                # 在用的备份配置与任务状态，不是备份包
│   ├── config.json
│   ├── github.token       # 私有仓库访问令牌
│   ├── recovery.key       # 备份包解密密钥，需另行保管
│   ├── status.json
│   └── heartbeat.json
├── gateway-captures/      # 加密交互记录，自动备份不包含正文
│   ├── *.request.enc      # 当前格式的请求侧加密标记
│   ├── *.response.enc     # 当前格式的响应侧加密标记
│   └── shared/            # 实际加密事件，同一用户去重
└── host-log-metrics.json  # 宿主机日志大小摘要
```

锁、排队文件及恢复事务仅在相应阶段出现；不要将 `data/backup/` 当作旧备份目录清理。`backups/`、`data/backups/`、`deploy-staging/` 或 `.deploy-*` 如由部署流程创建，属于人工暂存或回滚副本，不是程序必需目录，清理前应核对是否仍被使用。

四类密钥不能相互替代：

| 文件或凭据 | 用途 |
| --- | --- |
| 门户 `portal_app_secret` | 门户会话安全和交互记录加密，已有部署应保留原值 |
| CPA/CPAMP 管理密钥 | 管理接口鉴权，门户连接 CPAMP 的密钥必须匹配 |
| CPAMP `data.key` | 解密 CPAMP 数据库保存的连接凭据 |
| 备份 `recovery.key` | 解密 `.cbackup` 备份包，不能仅留在原服务器 |

运行中的 SQLite 不应只复制主文件当作完整备份，应使用一致性快照，或停服后按实际状态备份配套文件。若另行备份交互正文，必须同时保留原门户应用密钥。删除数据目录、命名卷或执行 `docker compose down -v` 会造成数据丢失。

## 日常运维

### 状态与更新

```bash
docker compose -f compose.prebuilt.yaml ps
docker compose -f compose.prebuilt.yaml logs -f --tail=100 cliproxy-portal
curl -fsS http://127.0.0.1:18080/healthz
```

`/healthz` 只表示门户 HTTP 服务可用，不代表 CPA、CPAMP 或额度全部正常；完整状态在“系统管理”查看。

更新前在构建机测试并编译，确认有可用的一致性备份及回退方式，避开正在运行的备份/恢复任务。上传新二进制为 `dist/portal-linux-amd64.new` 后：

```bash
chmod 755 dist/portal-linux-amd64.new
mv -- dist/portal-linux-amd64.new dist/portal-linux-amd64
docker compose -f compose.prebuilt.yaml up -d --no-deps --force-recreate cliproxy-portal
curl -fsS http://127.0.0.1:18080/healthz
```

启用网关时，所有 Compose 运维命令加上 `-f compose.gateway.yaml`。文件级绑定挂载更换后应重建容器，不能只 `restart`。源码构建部署使用 `docker compose -f compose.yaml up -d --build --no-deps cliproxy-portal`。涉及数据库迁移时，不要盲目恢复旧数据库覆盖新增数据。

### 存储占用与日志采集

页面将门户数据库、CPAMP 数据库、其他存储分别展示；数据库占用包括主文件、WAL 和 SHM。“其他存储”汇总交互记录、CPA 主日志、请求/响应日志及容器日志，不是整个服务器磁盘用量，也不把 Docker 镜像计算进去。

CPA 和容器日志大小由 `ops/collect-host-log-metrics.py` 采集；脚本只统计大小，不读取正文。摘要缺失或超过 5 分钟未更新时显示待采集。先按实际部署调整脚本容器名和 service 路径，再安装：

```bash
sudo cp ops/cliproxy-portal-log-metrics.service ops/cliproxy-portal-log-metrics.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cliproxy-portal-log-metrics.timer
sudo systemctl start cliproxy-portal-log-metrics.service
```

`ops/` 是运维脚本、任务配置和说明文档，不是实际备份包。systemd 示例默认使用 `/root/cliproxy-portal` 与 `/root/cpa-manager-plus`，部署到其他路径需同步调整。

## 环境配置

下表列出程序默认值；Compose 可能覆盖部分配置。`.env` 只替换 Compose 中显式引用的变量，不会自动将全部变量传入容器。新增配置需写入 Compose 的 `environment` 或覆盖文件，并重新创建容器。

| 变量 | 程序默认值 | 用途 |
| --- | --- | --- |
| `PORTAL_LISTEN_ADDR` | `:18080` | 门户监听地址 |
| `PORTAL_DATABASE_PATH` | `/data/portal.db` | 门户数据库 |
| `PORTAL_EXTERNAL_URL` | `http://localhost:18080` | 门户公网地址 |
| `CPA_API_BASE_URL` | `http://localhost:8317` | 展示给用户的模型 API 地址 |
| `CPAMP_BASE_URL` | `http://cpa-manager-plus:18317` | CPAMP 管理地址；Compose 默认改为 `host.docker.internal` |
| `CPAMP_ADMIN_KEY_FILE` | `/run/secrets/cpamp_admin_key` | CPAMP 管理员 Key 文件 |
| `PORTAL_APP_SECRET_FILE` | `/run/secrets/portal_app_secret` | 至少 32 字符的门户应用密钥文件 |
| `PORTAL_BACKUP_STATE_DIR` | 数据库同目录下 `backup/` | 备份配置、令牌、恢复密钥与任务状态 |
| `PORTAL_COOKIE_NAME` | `cliproxy_portal_session` | 会话 Cookie 名 |
| `PORTAL_TIME_ZONE` | `Asia/Shanghai` | 页面显示时区；备份计划固定按北京时间 |
| `CPAMP_TIMEOUT` | `15s` | 管理请求超时 |
| `PORTAL_RECONCILE_INTERVAL` | `5m` | Key 对账间隔 |
| `PORTAL_PENDING_RETRY` | `30s` | 待处理撤销任务重试间隔 |
| `PORTAL_USAGE_CACHE_TTL` | `2m` | 进程内用量缓存时长 |
| `PORTAL_REGISTRATION_OPEN` | `true` | 仅首次建库时的注册默认值 |
| `PORTAL_TRUST_PROXY_HEADERS` | `false` | 信任转发 IP 头；仅在受控代理后启用 |
| `PORTAL_GATEWAY_LISTEN_ADDR` | 空，关闭网关 | 可选网关监听地址 |
| `CPA_UPSTREAM_URL` | 空 | 启用网关时必填的 CPA 内部地址 |
| `PORTAL_GATEWAY_CAPTURE_DIR` | `/data/gateway-captures` | 加密交互记录目录 |
| `PORTAL_HOST_LOG_METRICS_PATH` | `/data/host-log-metrics.json` | 宿主机日志大小摘要路径 |
| `PORTAL_BINARY` | `portal-linux-amd64` | 预编译 Compose 的文件名替换变量，不是程序配置 |

## 开发与验证

### 个人请求日志查询

个人请求日志独立查询用户所有 Key（包括已撤销 Key）最近 100 条元数据，不计算完整用量汇总、时间线或模型排行；手动刷新绕过缓存。缓存按用户、Key 集合与历史起点隔离，正常复用时间由 `PORTAL_USAGE_CACHE_TTL` 决定。

CPAMP 1.14.1 的 Key 筛选使用 `coalesce(api_key_hash, '')` 表达式。对应查询较慢时，可在宿主机添加匹配表达式的索引（不修改记录、密钥或配置）：

```bash
# 先检查表结构并预览，再按实际宿主机路径执行
python3 ops/optimize-cpamp-request-indexes.py /var/lib/docker/volumes/cpamp_cpa-manager-plus-data/_data/usage.sqlite
python3 ops/optimize-cpamp-request-indexes.py /var/lib/docker/volumes/cpamp_cpa-manager-plus-data/_data/usage.sqlite --apply
```

脚本可重复执行；未知表结构或同名索引定义不符时会停止。索引随 CPAMP 数据库快照一起备份；恢复到早于此次优化的数据库后应重新执行。升级 CPAMP 后需重新核对其查询表达式，不能假定此优化适用于所有版本。

### 测试

```bash
go test ./...
go vet ./...
go build ./cmd/portal
node --test internal/webui/tests/*.test.cjs
python -B -m unittest discover -s ops -p 'test_optimize_cpamp_request_indexes.py'
```

前端测试使用 Node.js 内置测试运行器，不需要 `npm install` 或 `node_modules`，覆盖兼容开关自动保存与失败恢复、备份弹窗、私密下载、瀑布流排序、渠道切换、额度轮播、导航和图表手势。模拟测试不能替代真实浏览器的视觉与交互验证。

Windows PowerShell 枚举前端测试：

```powershell
$portalTests = Get-ChildItem internal/webui/tests -Filter "*.test.cjs" |
  ForEach-Object { $_.FullName }
Push-Location $env:TEMP
try { node --test $portalTests } finally { Pop-Location }
```

项目位于 OneDrive 时，不在同步目录创建、安装或链接 `node_modules`。第三方 Node 依赖处理应放在同步目录外的独立临时工作区，仅复制最终产物回项目。

Windows 网络诊断可使用 `ops/Run-PortalNetwork.cmd` 和 `ops/Test-PortalNetwork.ps1`；上传测试使用不存在的诊断模型，预期产生错误日志，不调用真实模型。使用方法见 [网络诊断说明](ops/portal-network-diagnostics.md)。诊断结果和备份可能包含敏感信息，不应公开上传。

### 源码结构

```text
cmd/portal/          程序入口、管理员初始化、备份 CLI
internal/backup/     一致性快照、加密上传、调度与恢复事务
internal/config/     环境配置
internal/cpamp/      CPAMP 管理接口客户端
internal/service/    账号、Key、额度、模型与预设逻辑
internal/store/      SQLite 存储与迁移
internal/gateway/    模型代理、加密交互记录与计时
internal/httpserver/ 路由、权限、系统检查与视图数据
internal/webui/      HTML、CSS、JavaScript 和前端测试
ops/                 日志采集、网络诊断和备份部署示例
dist/                构建产物，不纳入 Git
```
