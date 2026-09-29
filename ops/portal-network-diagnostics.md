# Windows 到门户的网络诊断

需要 Windows PowerShell 5.1 和系统 `curl.exe`（7.75 或以上，8.10 或以上能记录客户端最后字节发送时间）。不需要 Python、Node.js、管理员权限，不更改服务器、路由器、代理或网络配置。

将 `Test-PortalNetwork.ps1` 和 `Run-PortalNetwork.cmd` 放在同一文件夹，双击 CMD 即可运行。运行时输入**门户 API Key**，输入隐藏；不是管理 Key。结果保存在脚本文件夹下的新建 `portal-network-日期-随机ID` 文件夹中。也可以在 PowerShell 中执行下面的命令。

## 同一台电脑对比原网络与热点

在原网络执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Test-PortalNetwork.ps1 -Label original -CompareIp 212.135.210.56
```

然后切换手机热点，其他设置保持相同，再执行：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Test-PortalNetwork.ps1 -Label hotspot -CompareIp 212.135.210.56
```

`212.135.210.56` 是本次排查的门户源站 IP；以后源站变化时需要替换。`-CompareIp` 在保留原 Host 和 HTTPS SNI 的情况下固定连接地址，用来与正常 DNS 路径对比。它并不绕过系统 TUN。

脚本绕过 curl 配置文件、显式 HTTP 环境代理和系统 HTTP 代理。**Clash/VPN 的 TUN 仍可能接管连接**；报告会记录可能的代理进程、活动接口 MTU、DNS 结果和实际连接 IP。不要把存在代理进程本身当作已走代理的证明。为了对比未使用 Clash 的网络，可自行退出 Clash/TUN 后在两种网络下测试；脚本不会替你关闭它。

## 测试内容与日志影响

- DNS 解析、每个候选 IP 三次 TCP 握手、ICMP 探测。
- 模型列表小 GET，以及同一个 curl 进程中的三个连续 GET，观察连接复用。
- 1KB、64KB、**241877 字节（约 242KB）**的完整 JSON 上传，每个尺寸默认两次。
- 每次 HTTP 请求默认超时 60 秒，不重试、不跟随重定向。

上传调用 `/v1/chat/completions`，使用 `__portal_network_diagnostic_时间_ID` 这个不存在的模型名。**预期 HTTP 400 会写入 CPA 访问/错误日志**，它是诊断请求，不是真实模型故障。诊断不会调用真实模型，也不会提交你的聊天记录。请求大小使用公开随机填充内容构造；不存在模型由 CPA 拒绝。只在携带 Key 的模型列表返回 200 后进行上传测试。

预期 400 的响应必须包含本轮诊断模型名，报告才将 `diagnostic_body_confirmed` 设为 true。这说明应用解析到了该 JSON 请求，结合全长上传计数可检查完整请求的往返耗时；这不是独立的“门户接收完最后字节”时间戳。

只有连通性检查，不进行上传：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Test-PortalNetwork.ps1 -SkipUpload -NoPrompt
```

无 Key 时 HTTP 401 是正常的鉴权响应，只能证明 API 地址有响应，不能测出大请求上传质量。

## 报告

- `summary.txt`：每种路径、测试和大小的成功数、成功请求耗时中位数，以及解释。
- `timings.csv`：可用 Excel 打开的逐请求时间、状态码、IP、端口和 CPA trace ID。
- `report.json`：同样的明细，加环境和基础探测信息，适合后续服务器日志对照。
- `traceroute.txt`：只有添加 `-TraceRoute` 才生成，最长等待约 25 秒。

报告不保存 Key、完整请求头或实际聊天内容。Key 通过进程标准输入传递给 curl，不写入 curl 命令行或配置文件；也可通过已有进程的 `PORTAL_DIAG_API_KEY` 环境变量提供。请求体和临时响应在本次结束时删除。报告包含 IP、接口名、时间和诊断错误内容。

## 时间字段怎样理解

| 字段 | 含义 |
| --- | --- |
| `dns_ms` | 本次 curl DNS 耗时，可能命中系统缓存 |
| `tcp_ms` | DNS 完成到 TCP 建连的时间；复用连接通常为 0 |
| `tls_ms` | HTTPS 握手时间；目前 HTTP 入口为 0 |
| `request_ready_ms` | 从请求开始到准备传输的累计时间 |
| `local_send_complete_ms` | 从请求开始到 libcurl 发送最后一个字节的累计时间 |
| `local_send_phase_ms` | 准备传输到 libcurl 发送最后一个字节的时间 |
| `after_local_send_to_first_byte_ms` | 上述最后字节发送时刻到收到首字节的时间 |
| `first_byte_ms` | 从请求开始到收到第一个响应字节的累计时间 |
| `response_receive_ms` | 首字节到 curl 接收结束的时间 |
| `total_ms` | 整次 HTTP 请求的累计耗时 |

`local_send_complete_ms` **不是服务端收齐时间或 TCP ACK 时间**。即使它很短，请求数据仍可能在系统发送队列或网络中。`after_local_send_to_first_byte_ms` 包含后续网络传输、代理、服务器处理和响应返回，不能直接称作“服务器等待时间”。旧版 curl 没有最后字节指标时对应字段留空，不猜测。超时/失败的未完成阶段可能显示 0 或空值。

## 判断依据

- 原网络出现慢上传/超时，而热点同一命令稳定：支持问题与原网络路径有关，仍需区分电脑设置、路由器、运营商、丢包和限速。
- 固定 IP 路径显著改善：优先检查 DNS、解析结果与代理 fake-IP；仍不能直接断言 DNS 是唯一原因。
- 小 GET 很快、大请求很慢，且得到预期诊断 400：复现了**无需模型推理就慢**的情况。服务端处理和往返链路都在测量范围内，需要服务端读完请求时间或抓包进一步定位。
- 两种网络的诊断都很快，只有 Codex 慢：检查客户端实际代理路径、请求大小/协议和真实模型调用；不能据此证明所有时段网络都正常。
- 首次 GET 慢、复用 GET 快：建连阶段值得关注，但连续三次 GET 不等于长时间 SSE 连接测试。
- 401/403：鉴权问题；404/重定向：地址/入口问题；400 只有匹配诊断模型名才视为预期。
- ICMP 不通、traceroute 星号不等于实际 TCP 丢包。脚本不采集包级重传，不能仅靠客户端结果确定是哪一跳故障。

拿两轮的整个报告文件夹做对比；服务器可按 `run_id`（也在诊断模型名和请求头中）、本地带时区时间、`cpa_trace_id` 和来源端口关联日志。失败请求可能没有 CPA trace ID，也未必出现在全局用量日志中。

可选参数：`-BaseUrl`、`-Label`、`-CompareIp`、`-Repeat`、`-TimeoutSeconds`、`-UploadBytes`、`-OutputDirectory`、`-SkipUpload`、`-TraceRoute`、`-NoPrompt`。每轮输出目录必须是新目录，防止覆盖旧报告。

时间指标定义见 [curl 官方手册](https://curl.se/docs/manpage.html#-w)。
