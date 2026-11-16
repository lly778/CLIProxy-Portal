# AGY 客户端摘要与门户原始诊断（2026-11-16）

## 最终行为

- 客户端保持既有错误摘要与 error.code，普通 JSON 和 SSE response.failed 均不附带 CLI 原始诊断。HTTP 状态、错误分类、重试和恢复判定继续使用原映射。
- 门户失败详情优先显示 CLI 经当前请求的 ACP 通知或 RPC error.message 返回的原始诊断，保留原始大小写、换行和本轮尚未恢复的重试诊断。未知 RPC 错误也保留原始原因，常见凭据脱敏。没有原始诊断或为桥接本地校验错误时，继续使用原英文归类说明。
- CLI 诊断通过内部认证 worker 响应的私有 trailer 传给网关，网关将其加入请求失败详情的 cli_diagnostic 字段。私有 trailer、诊断正文不转发到客户端，不进入通用 Error.Error/JSON 序列化；已有失败记录不批量改写。
- 诊断最多 64 KiB；超过时保留开头和末尾，并明确标记截断。内部连接读缓冲允许完整接收经过 base64 编码的诊断。响应重放路径同样保留私有诊断；不读取整份 CLI 日志或其他会话 stderr，也未接入 RPC error.data。
- 中间错误通知不结束、不重发原生 Prompt；最终失败才提交诊断，重试成功后丢弃旧诊断。完整会话保存时机仍为移出内存、关闭、中断等原边界，未增加每次交互快照。

## 验证

- go test ./...、go vet ./...、git diff --check 通过；最终调整后 AGY/gateway/httpserver 回归通过。
- 原始诊断、脱敏、大小上限、JSON/SSE、内部真实 HTTP 传输、响应重放、客户端无诊断泄漏、门户优先显示和原生重试等核心回归连续 10 次通过。64 KiB 诊断经真实私有 HTTP 连接收取，客户端正文保留摘要；网关不转发私有头和 trailer。
- 未发起真实模型调用或客户端文档工具业务验收。

## 发布证据

- 实现提交：025c7bf85d04a78cf7957d089c4858c66716dfeb，已推送 main。
- Linux amd64 / CGO=0 / trimpath / -ldflags '-s -w' 构建：19,566,752 bytes；SHA256 eba4f2ec503d77f2a441994eaa7e8e4cfbdeff1d10e28c01824b8f33be3bcd0b。vcs.revision 对应实现提交，未跟踪的 ops/__pycache__/ 导致 vcs.modified=true，未将其纳入提交。
- 本地产物：C:\Users\lly77\AppData\Local\Temp\codex-agy-diagnostic-release-c26cff68a4b54f379d39db1c53ef918e\portal-linux-amd64。
- 完整 xz 为 6,108,280 bytes，上传中断后改用 411,737-byte BSDIFF40 增量包。增量包 SHA256 b2f5184d77813a48b8e875fcb73152e4858ff89d71b1996ce9fd581c5b19c6b2；本地与服务器均通过解码还原，并核对旧程序哈希、增量包长度/哈希及新程序完整长度/哈希。服务器仅用 Python 标准库还原，不安装运行时依赖。
- 初期 AGY 无活跃/等待任务，但 CPA 持续传输；多次空闲监测未满足 27 秒窗口，未切换。随后用户明确要求“直接重启吧，更新门户吧”，据此跳过空闲等待，原子替换并仅重建门户。没有创建备份或回滚副本。
- 门户容器 1c5587e30cf4 于 2026-11-16 16:54:12.885 +0800 启动；16:54:46 验证 healthy。实际 portal 子进程与挂载程序哈希均匹配上述新产物。
- 服务器本地、门户公网和 API 公网 /healthz 全为 200；客户端公网两处检查也为 200。启动 ERROR/panic/fatal 计数 0。
- 会话缓存 late-client-result-routes-v6，磁盘记录 272、ready 记录 57，无 cache_error；AGY not_connected/sessions=0 是重启后的惰性状态。
- CPA 6a99f7e729f7（2026-10-22T05:50:39.056779009Z）、CPAMP 688354d0aead（2026-11-14T19:01:36.313887916Z）ID 与启动时间保持不变。CLI SHA256 仍为 8b4fc29f938cab08c47e7471238184fe77a6c9757e0d640bf980f133998a6f7c，目录刷新实验 false；客户端及输入预算未修改。
- 远端暂存程序已原子替换消失。部署细节仅记录在本交接文档，开源 README 只描述通用行为。
