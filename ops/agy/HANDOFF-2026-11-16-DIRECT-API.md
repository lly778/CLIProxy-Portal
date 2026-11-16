# 同步脚本改用直连 API：已部署（2026-11-16）

用户要求将同步脚本写入客户端的 API 地址改为直连入口。门户从当前 CPA_API_BASE_URL 下发 api_base_url，脚本不硬编码站点地址。

- 服务器 /root/cliproxy-portal/.env 中 CPA_API_BASE_URL 已由 https://api.lly778.xyz 改为 http://cpa.lly778.xyz:8317，保留原文件权限和所有者，原子写入；本次重建门户后已生效。
- 本地 internal/clientsync/Sync-Codex.ps1 的 Assert-URL 新增 API 专用远程 HTTP 支持；模型配置获取处启用，PortalUrl 校验不变。保留 UTF-8 BOM，兼容 Windows PowerShell 5.1。BAT 自动内嵌同一源脚本。
- 本次已同时部署脚本修改和 API 地址配置，避免旧同步脚本拒绝新 HTTP 地址。
- 已验证公开 /api/client-model-config 返回 api_base_url=http://cpa.lly778.xyz:8317/v1；下载脚本继续从原门户地址取得模型配置。门户 Key 页的 API 地址也读取 CPA_API_BASE_URL，因此随配置一起切换。
- PowerShell 语法与 API HTTP/HTTPS、凭据/查询参数/非法协议拒绝、门户 HTTP 继续拒绝等检查通过；go test ./internal/clientsync ./internal/httpserver 通过。未修改本机真实 Codex 配置，未发起模型调用。
- 实现提交 bb5753e5b6b684b673369119420b1c740b9848d4 已推送 main；无关 ops/__pycache__/ 保持不动。


## 发布验收

- Linux amd64 / CGO=0 / trimpath / -ldflags '-s -w' 构建 19,566,752 bytes，SHA256 03860aab8b8850cb07f371a23a8f00c093d66d8ba0f82f24aec18fb16dfc750e。vcs.revision 对应实现提交，未跟踪交接文档及 ops/__pycache__/ 导致 vcs.modified=true，未编入无关文件。
- 本地产物 C:\Users\lly77\AppData\Local\Temp\codex-direct-api-release-6c322330d71b45e2beff5769df070433\portal-linux-amd64。传输 BSDIFF40 增量包 44,466 bytes，SHA256 d7cfdf4008488f1a7bba06e60f7f4d6894480c09b0aafc21ef4680eabfc16ba4；本地及服务器还原校验，新程序完整哈希匹配后原子替换。
- 发布前 AGY 无活跃或等待任务。沿用用户此前直接更新门户的偏好，未再等待 CPA 流量空闲窗口；只重建门户，不创建备份或回滚副本。
- 新容器 2f51aa5e7c15 于 2026-11-16 18:36:18.042 +0800 启动。18:37:38 验收 healthy，实际 portal 子进程及挂载文件哈希均匹配新程序；服务器本地、门户公网、API 公网 /healthz 全为 200，启动 ERROR/panic/fatal 计数 0。
- 服务器本地与客户端公网读取同步配置，均确认 api_base_url=http://cpa.lly778.xyz:8317/v1，模型数量 4。PS1 下载内容 SHA256 0379dff53fef96969ffb794a4cb578a20ae9f1a880135b4a435e3b51ecf83a44，与发布源脚本注入原门户地址后的内容完全一致；下载 BAT 为 42,019 bytes，解码后内嵌脚本与 PS1 相同且只有一个 UTF-8 BOM。没有运行用户真实 Codex 配置同步，也没有发起模型业务调用。
- CPA 6a99f7e729f7（2026-10-22T05:50:39.056779009Z）、CPAMP 688354d0aead（2026-11-14T19:01:36.313887916Z）容器 ID 与启动时间不变。AGY CLI SHA256 8b4fc29f938cab08c47e7471238184fe77a6c9757e0d640bf980f133998a6f7c 不变，目录刷新实验 false。
- 会话缓存 late-client-result-routes-v6，磁盘记录 267、ready 记录 57，无 cache_error。重启后 AGY not_connected/sessions=0 是惰性连接状态，未改变保存策略或上下文预算。
