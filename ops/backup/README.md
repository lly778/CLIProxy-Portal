# 加密备份与恢复

门户显示备份状态及“备份”“恢复”两个按钮。“备份”弹窗内可设置 GitHub 私有仓库和访问令牌、自动备份开关、备份星期和北京时间（可选择时、分），并下载备份恢复密钥。保留份数、备份来源及部署策略仍由服务器管理员在后台配置。加密、上传、停服及数据切换由宿主机任务执行；门户容器无需 Docker socket 或上游目录挂载。

## 备份范围与两种恢复

备份包含门户 SQLite 和 CPAMP `usage.sqlite` 一致性快照、CPAMP `data.key`、CPA 的 `config.yaml` 和 `auths`，以及用于重新部署的 Compose、`.env`、管理密钥及门户应用密钥。来源由受保护的 `sources.json` 列出，限定允许的文件名称，不允许网页选择任意服务器文件。必须来源缺失、链接或读取失败时，不会发布成功备份。

`data.key` 用于解密 CPAMP 数据库保存的 CPA 连接凭据，不是登录管理密钥，也不是加密备份包的恢复密钥；它必须与 `usage.sqlite` 成套保留。宿主机核对 CPAMP 的卷、数据库及密钥路径后暂停 CPAMP，取完快照立即恢复并检查容器健康；压缩、加密、验证和上传在恢复运行之后执行。门户和 CPA 不因备份暂停。SQLite `VACUUM INTO` 包含已提交的 WAL 数据，因此备份包使用独立数据库快照，不另外复制在线 WAL/SHM。

| 操作 | 覆盖内容 | 服务处理 |
| --- | --- | --- |
| 恢复 | 仅门户 SQLite | 停止门户、替换数据库、重新启动并验证；不操作 CPA/CPAMP |
| 重建并恢复 | 门户 SQLite、CPAMP usage.sqlite/data.key、CPA 配置与 auths、上游 Compose/.env、CPA/CPAMP 管理密钥及门户连接 CPAMP 的管理密钥 | 先下载备份记录的固定镜像摘要；停止门户和上游，再重建 CPA/CPAMP、启动门户并检查健康 |

普通恢复不会还原 CPA 中的原始 API Key、模型别名、思考强度上限或模型启用状态。数据库中的 Key 元数据、账号、预设、布局和门户统计会恢复到快照时间；运行后仍按当前上游配置正常对账。重建并恢复会把上述上游配置也恢复到备份时间，包括原始 API Key。过期或被撤销的 OAuth 授权仍需重新登录。

两种在线恢复都会清空备份中的登录会话、密码重置码和待执行同步任务，避免旧会话复活或重放过时上游操作。需要重新登录。门户应用密钥只为全新服务器初始化保留，在线恢复不轮换当前应用密钥；门户本身的 Compose/.env 不由网页恢复覆盖。

不备份 `usage-archives`、门户独立交互正文、普通日志、镜像文件、GitHub 令牌或备份恢复密钥。CPAMP 数据库内仍在线的请求记录、用量统计、价格、Key 别名及设置随快照保留；门户内部审计、计时样本也保留。CPAMP 数据库本身若存有敏感请求字段，它们仍属于加密数据库快照。重建恢复替换 CPAMP 数据库和 data.key，不合并新旧统计、不删除现有归档目录或 manager.lock。DNS、Cloudflare 隧道凭据、防火墙、Docker 和门户程序需另行准备。

本方案按要求排除归档文件，并非启用历史归档后的完整 CPAMP 归档灾备。如果数据库含已有归档任务/segment 索引，备份仍保留原始数据库，不擅自删除归档索引或 identity ledger；自动重建恢复会在停服前拒绝该包，提示管理员处理配套归档，避免重新启动缺少文件的维护任务。普通门户恢复不受影响。参考 [CPAMP 官方备份说明](https://seakee.github.io/CPA-Manager-Plus/docs/operations/backup.html)。

## 安装和批准部署档案

示例对应门户 `/root/cliproxy-portal`、上游 `/root/cpa-manager-plus`。先核对实际路径、容器及 Compose 服务名，不要直接套用于其他部署。

1. 将 `sources.example.json` 按实际路径调整，保存为 `/etc/cliproxy-portal-backup/sources.json`。
2. 将 `restore.example.json` 按实际路径调整，保存为同目录的 `restore.json`。填写正在使用的上游 Compose 的 SHA256、两个批准的镜像仓库、健康检查地址和管理密钥文件路径，以及 CPAMP 命名卷名称、宿主机 usage.sqlite/data.key 路径。用 `docker volume inspect <卷名>` 核对实际挂载目录，不要假定卷名和示例相同。普通恢复只需要数据库、门户容器及健康地址；重建和含 CPAMP 的备份需要全部字段。
3. 策略目录由 root 所有且权限 0700，JSON 文件由 root 所有且权限 0600。上游 Compose 及其父目录也必须由 root 控制，不能允许组或其他用户写入。
4. 数据库目录所有者与门户 UID/GID 相同（预编译部署为 10001:10001）；备份状态目录为同一所有者、0700。恢复目标的父目录须预先建立，不能通过符号链接重定向。
5. 安装本目录的宿主机 service/timer。service 需要读来源文件、访问本机 Docker，写门户 data/secrets、上游部署目录及 CPAMP 数据卷；修改路径时同步修改其 `ReadWritePaths`。三个配置文件应一并升级，旧来源配置不会自动把未列出的 CPAMP 文件加入包内。

```sh
sudo install -d -m 700 /etc/cliproxy-portal-backup
# 先调整示例文件，并用 sha256sum /root/cpa-manager-plus/compose.yaml 填写批准摘要。
sudo install -m 600 ops/backup/sources.example.json /etc/cliproxy-portal-backup/sources.json
sudo install -m 600 ops/backup/restore.example.json /etc/cliproxy-portal-backup/restore.json
sudo install -d -m 700 -o 10001 -g 10001 /root/cliproxy-portal/data/backup
# 全新主机先准备批准的数据卷，确保 systemd 启动时可设置该目录的写权限。
sudo docker volume create cpamp_cpa-manager-plus-data
sudo install -m 644 ops/backup/cliproxy-portal-backup.service /etc/systemd/system/
sudo install -m 644 ops/backup/cliproxy-portal-backup.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cliproxy-portal-backup.timer
```

重建使用管理员批准的 Compose，而不执行任意上传的部署文件。备份中的 Compose 必须与批准模板完全一致；展开 .env 后的端口、挂载、命令、环境及权限结构也必须与后台档案一致。镜像仅接受批准仓库的 `@sha256` 固定摘要，不自动追踪 latest。模板或结构改变需管理员先审查和重新批准。管理密钥文件可恢复，但门户/CPAMP 的密钥必须相同，CPA 配置中的管理密钥必须匹配。

安装策略后，新备份会自动从正在运行的两个上游镜像记录仓库摘要。未包含两项镜像摘要或 CPAMP usage.sqlite/data.key 的旧备份不能自动重建，仍可普通恢复。备份期间上游镜像摘要无法确认会失败；不要在凭据或模型配置变更途中备份，跨服务文件并非同一数据库事务。

两个上游容器均存在或均缺失时可以执行重建；仅缺一个时会拒绝，需管理员先核对残留部署。容器必须属于批准的 Compose 项目和服务；数据卷不得被其他容器共享，数据库与 data.key 必须使用批准的命名卷。镜像下载失败会在停服前退出。

## 后台仓库和密钥设置

也可在系统管理页点击“备份”，展开“备份设置”：填写仓库 `owner/repo` 与 GitHub 访问令牌，选择是否启用每周自动备份、备份星期及北京时间（00:00–23:59），验证管理员密码后下载独立恢复密钥；另存保管并勾选确认，再验证密码保存设置。已有令牌留空保持不变，不会回显。保留份数与实例标识保持不变，首次显示默认每周一北京时间 03:00、保留 3 份的计划。保存设置不会排队手动备份；启用自动备份后已过本周计划时间且尚未执行时，宿主机下一次检查可能补执行。下载沿用已有恢复密钥，不轮换。设置保存及密钥下载均需管理员、CSRF 和密码验证，并记录不含秘密内容的操作日志；备份/恢复运行或排队期间禁止修改。来源及恢复策略不能通过网页更改。

新建专用 GitHub 私有仓库，并用 README 初始化。fine-grained token 仅授予该仓库 Contents 读写权限，通过 0600 文件传入，不放进聊天、命令参数或日志。恢复密钥独立于应用密钥，必须另存到其他设备；不会进入备份包。

```sh
/root/cliproxy-portal/dist/portal-linux-amd64 backup key \
  --state-dir /root/cliproxy-portal/data/backup \
  --output /root/portal-recovery-export.key

/root/cliproxy-portal/dist/portal-linux-amd64 backup configure \
  --state-dir /root/cliproxy-portal/data/backup \
  --repository lly778/CLIProxy-Portal-Backups \
  --token-file /root/private-backup-token \
  --weekday 1 --hour 3 --minute 0 --retain 3 --enabled=true --key-saved
```

密钥导出目标必须是新文件且在共享备份目录外；已有密钥不会轮换。确认另存后才使用 `--key-saved`。更新配置可省略 `--token-file` 沿用令牌。`--enabled=false` 只停用每周自动调度，不取消运行或排队任务。默认每周一北京时间 03:00、保留最近 3 份；`--weekday` 为 0–6（0 周日，1 周一）。一周按北京时间周一至周日计算，本周计划时间已过且尚未尝试时，下一次 tick 补执行一次；失败不在本周循环重试，手动备份仍可随时请求。

## 门户操作与失败回退

备份和恢复均需要管理员密码和 CSRF 验证。恢复弹窗选择模式，勾选覆盖确认，上传加密的 `.cbackup`；不能填写服务器目标路径。默认使用后台恢复密钥，新服务器也可在弹窗提供独立保管的密钥，不保存为后台配置。未确认或校验失败不会覆盖数据。

完整加密认证、清单 SHA256、SQLite 结构及有效管理员检查通过后，先将候选文件暂存到目标所在文件系统。停止服务后生成一致性回退副本，再替换数据库及所选模式的其他文件。auths 整目录替换，不与旧授权文件合并。切换或健康检查失败时尝试恢复原文件及原镜像版本；回退失败保留事务记录，不把任务标为成功。

回退副本保存在 `restore-rollback-<随机标识>` 的 root 私有目录中，页面只显示相对标识，无明文下载。副本不纳入定时备份，也不会自动删除，管理员确认恢复结果后再清理。上传包、临时密钥和解密工作目录在任务结束后删除。备份/恢复共享运行锁，不允许同时执行。

异常中断不会盲目清锁或抹掉事务。先确认宿主机任务已结束，检查 `runner.lock` 及 `.database-restore-transaction`；未完成事务由下一次 runner 按记录完成健康检查或回退。只有确认没有仍运行的任务，才可人工移除明确的旧锁。不要手工删除未完成事务或回退目录。

## 加密、上传和保留

流程为 SQLite `VACUUM INTO` → `quick_check` → 文件 SHA256 清单 → gzip → 分块 AES-256-GCM → 本地解密验证 → 确认私有仓库 → 草稿 Release 上传 → 下载 SHA256 验证 → 发布 → 清理旧备份。

每个 Release 仅包含加密的 `backup.cbackup`，标题为“门户备份”，标签使用 `portal-backup-` 前缀，说明标识为 `CLIProxy Portal backup v1`；不再生成或上传恢复脚本。旧版备份仍可恢复，新旧发布按时间统一计算保留份数。认证帧与末尾标记检测损坏、乱序和截断；加密文件小于 GitHub 单附件 2 GiB 限制，解压总量限制为 8 GiB、最多 10000 文件。压缩直接流向加密输出，生成快照的临时副本在验证前释放。备份和恢复在停服前检查磁盘余量，为快照、解密验证及失败回退预留空间；容量不够会拒绝，不会自动删除现有数据或回退副本。只清理本安装标识的成功备份 Release 和 tag，不碰其他安装、软件发布或源码。失败不会清理旧成功备份。

如果快照任务异常退出，下一次 tick 通过独立的宿主机文件锁确认没有仍在取快照的进程，再启动批准的 CPAMP 容器。暂停记录位于 `.manager-backup-paused`；启动或健康检查失败会保留记录。主任务遗留的 `runner.lock` 不会盲目删除，确认任务结束后由管理员处理。

## 全新服务器首次准备

网页一键重建需要一个已经运行的门户、Docker/Compose 和管理员批准的后台策略。整台服务器丢失后，不能在尚不存在的门户里点击按钮。

取回对应版本门户二进制、加密包及另存的恢复密钥，使用原生离线解包命令准备数据。它不读取线上配置、不连接 GitHub、不启动服务，只写尚不存在的目录；不提供额外恢复脚本。

```sh
/path/to/portal-linux-amd64 backup restore \
  --file /path/to/backup.cbackup \
  --key-file /path/to/portal-recovery.key \
  --output /root/recovered-portal
```

检查得到的 `portal/`、`upstream/` 和 `manifest.json`，按实际路径准备门户应用密钥、数据库、连接密钥、部署参数及文件权限，清空旧 sessions/password_resets/sync_jobs。先完成门户和后台策略初始化，隔离对旧上游的访问，再使用“重建并恢复”重新部署 CPA/CPAMP。Docker、域名/隧道及网络需另外配置，GitHub 令牌和恢复密钥需重新设置。不要执行 `docker compose down -v` 或覆盖未经确认的现有数据。
