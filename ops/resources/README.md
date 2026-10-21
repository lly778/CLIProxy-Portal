# CPA 套件资源保护与监测

入口：**系统管理 → 资源保护**。默认观察模式，只记录并放行，不修改任何容器上限。点击“设置”打开弹窗，管理员登录后直接保存设置，保留权限与 CSRF 校验；过期页面不能覆盖新的设置。

## 总量预算与实时显示

- 实时显示整机 CPU 使用率及门户、CPA、CPAMP 三个容器合计的内存。内存卡片主数字显示“已用 / 套件上限”（MiB），上限为 0 时显示“不限”；下方单独标明整机可用/总内存。页面可见时每五秒刷新，不覆盖未保存的表单。
- 默认全局并发 2、每用户并发 1、单请求 10 MiB、整机 CPU 90%、套件内存 768 MiB。所有数值项均支持填 0 表示该项不限。非零内存上限须至少低于整机总内存 64 MiB。内存使用 cgroup v2 的 `memory.current - inactive_file`，扣除可回收的非活跃文件缓存；整机可用内存单独来自 MemAvailable，不与套件内存相减。
- 三个容器须使用同一个 `cliproxy.slice` 父组，以便合计内存。其他程序、隧道和宿主机备份任务不计入套件内存，但计入整机 CPU。CPU 使用 `/proc/stat` 整机累计计数的增量，空闲时间包含 idle 与 iowait，不重复计入 guest 时间，全部核心满载为 100%；4 核机器上占满一个核心约为 25%。CPU 达到或超过上限并持续 15 秒，或套件内存达到或超过上限时暂停新模型请求；恢复条件见下文。启用 CPU 或内存保护时，监测十秒未更新会暂停新请求。
- CPU 和内存预算用于控制新请求准入。已有请求继续处理，因此短时仍可能超限，无法保证完全杜绝 OOM；不设置单个容器的 CPU/内存限制。
- 两种门户 Compose 模板均不设置单个容器限额。父级 slice 仅开启资源统计，不设置 CPUQuota 或 MemoryMax。资源保护设置保留在数据库中，升级不会擅自改变原有阈值或观察/保护模式。
- 策略随门户数据库备份恢复；“近 7 天内的保护记录”按持久化资源保护事件统计最近 168 小时的拒绝和观察放行次数，门户重启不清零，不受请求日志最近 100 条限制。统计读取失败时显示“—”，删除对应事件会同步减少计数。
- 每次保护触发均单独进入个人请求日志、管理员用户详情及全局日志，时间格式沿用既有请求日志，不合并记录，记录不含请求内容或完整 Key。拦截显示“已拦截”、具体原因和已知 HTTP 状态码；观察模式显示“观察放行”。状态标签保留中文，下方资源保护原因统一英文，与英文网关错误保持一致；历史记录也采用该显示方式。未知模型、Token 及耗时显示“—”，这些保护事件本身不额外计入模型用量或平均耗时。历史事件沿用原有存储，调整展示位置不清零七天统计。
- 保护事件不再出现在系统日志中，资源保护设置修改仍保留。系统日志不显示“结果”和“来源”列；关联对象居中，详情左对齐并完整换行。
- CPU、内存超限或监测不可用时，资源保护区域使用红色提示条显示原因。保护模式说明新请求暂停、已有请求继续及自动恢复；观察模式说明仍会放行。恢复正常后隐藏提示，不显示“正常”徽标。

## 暂停后的恢复条件

| 预算 | 暂停条件 | 恢复阈值 |
| --- | --- | --- |
| CPA 套件内存 | 合计用量达到或超过上限 | `max(内存上限 − max(32 MiB, 内存上限 × 5%), 内存上限 × 50%)` |
| 整机 CPU | 整机使用率达到或超过上限，持续 15 秒 | `max(CPU 上限 − 15 个百分点, CPU 上限 × 50%)` |

暂停后，启用的两项预算都必须回落到各自恢复阈值或以下，并连续保持 10 秒，才恢复接收新请求；填 `0` 的项目不参与判断。任一项超过恢复阈值，或采样无效、采样间隔超过 10 秒，都会重新累计稳定时间。后台每 2 秒采样，恢复判断随采样更新，不要求已有任务全部结束。

例如内存上限 8192 MiB、CPU 上限 90% 时，套件内存须不高于 7782.4 MiB、整机 CPU 须不高于 75%，两者同时满足并持续 10 秒。即使只是内存触发暂停，也须满足 CPU 的恢复条件。

恢复阈值低于触发阈值，并增加稳定等待，是为了减少上限附近波动引起的反复拦截与放行。内存恢复缓冲取上限的 5%，最少 32 MiB，恢复阈值不低于上限的一半，避免小预算下阈值过低。恢复缓冲并不预估后续请求需要多少内存；已有请求继续运行，新图片请求也可能使资源再次超限。

## 最近 60 分钟趋势

整机 CPU 与套件已用内存在同一张图中展示，共用当前时刻向前滚动 60 分钟的时间轴。CPU 使用左侧百分比坐标，内存使用右侧 MiB 坐标，不按整点分段。悬停或聚焦图表后使用左右方向键，可同时查看两项指标及具体采样时间。

复用每两秒的资源采样，每 10 秒分别保留 CPU 与内存的峰值；同一时间桶内的两个峰值不一定发生在同一瞬间。图表每 15 秒刷新，页面隐藏时暂停请求，刷新失败时保留上次曲线并提示。监测中断处不连线，切换为整机 CPU / 套件内存口径后从实际采集时刻开始累计，不拼接旧口径的数据。

内存中最多保留 361 个时间桶，每分钟及正常退出时将最近一小时样本保存到门户数据库的 `resource_history_host_cpu_suite_memory_v1` 设置项。旧 `resource_history_v1`、`resource_history_suite_v1` 保留但不用于当前图表。重启后恢复已保存的有效样本；异常退出可能丢失最后一分钟未保存的样本。历史接口仅管理员可访问。

## 请求保护

门户 Key 按用户合并计数，其他 Key 共用一个额度。POST `/v1/`、`/v1beta/`、`/backend-api/`、已支持的推理路径和 WebSocket 升级受保护；登录、管理、健康检查不占额度。SSE 和 WebSocket 在整个请求/连接期间持有额度，取消及上游错误释放；全局并发为非零时，后台交互记录达到全局并发加四会暂停新请求。

并发超额返回 429；总资源压力、监测中断、记录积压返回 503，附 `Retry-After: 5`。请求传输大小或任一解压阶段超限返回 413，无效压缩返回 400，未支持编码返回 415。客户端仍应遵守重试间隔。

启用保护时先用临时文件检查大小，再向 CPA 转发原传输字节，支持 identity/gzip/deflate/zstd（最多三层）。WebSocket 按完整消息（含分片）检查大小并关闭压缩协商；超限连接终止。观察模式只检查传输大小、并发及资源压力，不额外解压或解析 WebSocket 消息。

## 轻量监测

门户后台每两秒读取 `/proc/stat` 的整机 CPU 计数及父级 cgroup 的 `memory.current`、`memory.stat`，通过 `/proc/meminfo` 获取整机容量与可用内存。实时状态和固定容量趋势缓冲保存在内存，不启动额外采集进程、不向门户挂载 Docker socket。CPU 计数器重置后重新预热，套件内存监测失败不回退为整机内存。

适用于 Linux cgroup v2、Docker systemd cgroup driver。父组只读挂载到门户 `/run/cliproxy-suite`；设置 `PORTAL_RESOURCE_CGROUP_PATH` 可调整程序读取路径。`PORTAL_RESOURCE_CGROUP_PARENT` 和 `PORTAL_RESOURCE_CGROUP_HOST_PATH` 可调整 Compose 的父组和宿主机只读挂载路径，两者必须对应。无法取得有效采样时显示监测不可用，保护模式暂停新请求。

### 安装与升级

先安装并启动资源统计 slice：

```sh
sudo install -m 644 ops/cliproxy.slice /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cliproxy.slice
```

门户 Compose 已包含父组和只读挂载。CPA/CPAMP 在原 Compose 上叠加本仓库的 `compose.resources.yaml` 并重建两个服务（需替换为实际文件路径和原有文件组合）：

```sh
docker compose -f /path/to/upstream/compose.yaml -f /path/to/portal/compose.resources.yaml up -d --no-deps --force-recreate cli-proxy-api cpa-manager-plus
```

重建门户后，通过 `docker inspect --format '{{.HostConfig.CgroupParent}}' <容器名>` 确认三个容器都是 `cliproxy.slice`。自定义父组须同步设置后台 `restore.json` 的 `resource_cgroup_parent`；默认同为 `cliproxy.slice`。在线重建恢复通过受保护的后台策略将两个上游加入父组，原批准的 Compose 模板和摘要无需改变。恢复后的容器仍纳入套件统计。原有阈值与模式保留。

接口说明参考 [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)。

## 可选：ESXi 物理宿主机监测

部署 `ops/collect-esxi-metrics.py` 和 `ops/resources/cliproxy-esxi-metrics.service`，在 Linux 服务器上的 `/etc/cliproxy-esxi/esxi.json` 保存 HTTPS 管理地址、用户名、密码及 `certificate_sha256`。目录权限为 0700，配置文件为 0600。采集器通过 systemd LoadCredential 读取配置，每五秒复用会话读取 ESXi HostSystem 的只读统计，固定验证服务器证书 SHA256 后才发送凭据。门户容器只读采集结果，不接触 ESXi 凭据。

配置 `PORTAL_HOST_METRICS_PATH=/data/esxi-metrics.json` 后重建门户：

- CPU 卡片、CPU 保护和 CPU 趋势使用 ESXi 物理宿主机用量；以 `overallCpuUsage / (cpuMhz × numCpuCores)` 换算百分比。全部物理核心满载对应 100%；超出 100% 的原始 MHz 仍保留在采集文件，保护使用率最高按 100% 判断。
- 内存卡片主数字和内存保护继续使用三个套件容器的合计内存，恢复缓冲规则不变，配置上限仍相对于当前虚拟机容量校验。
- 卡片下方“宿主机可用 / 总内存”来自 ESXi，未用量为物理总内存减 `overallMemoryUsage`；它与 Linux MemAvailable 的可回收缓存口径不同。
- 趋势图的 CPU、内存折线均来自 ESXi，右侧内存纵轴为物理宿主机总内存。图表内存不用于套件内存保护。单独使用 `resource_history_esxi_host_v1` 保存历史，不拼接此前虚拟机或套件趋势。
- ESXi 原始统计刷新与网页显示可能不同步，每五秒轮询并不代表底层指标每五秒更新。采集时间超过十五秒或数据无效时，不回退为虚拟机 CPU；保护模式按监测不可用暂停新请求。

配置示例（真实密码和配置不得提交到仓库）：

```json
{"url":"https://esxi.example","username":"monitor","password":"replace-locally","certificate_sha256":"replace-with-verified-sha256"}
```

现有 service 路径适用于 `/root/cliproxy-portal`。迁移或恢复到新服务器后，需要单独重新安装采集器和本机凭据配置；ESXi 凭据不会自动加入门户的 GitHub 备份。

指标说明参见 [ESXi HostListSummaryQuickStats](https://developer.broadcom.com/xapis/vsphere-web-services-api/latest/vim.host.Summary.QuickStats.html)。
