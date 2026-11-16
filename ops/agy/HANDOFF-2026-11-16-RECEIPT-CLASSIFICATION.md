# 客户端回执分类（2026-11-16）

用户核查今早 111 个工具回执后，要求识别常见文本格式的实际返回状态，随后明确要求部署推送。

## 实现范围

- 只对已知客户端 `exec_command`、`write_stdin`、`apply_patch` 及其 functions 命名空间使用文本格式适配。识别 Output/Final output 之前的标准元数据，不用命令正文内打印的退出码覆盖外层结果。
- 后台回执 Outcome 可为 success（退出码 0）、failed（非零退出或明确返回失败）、running、not_executed（策略/参数校验拒绝）、completed_with_errors（退出码 0 但含 PowerShell 异常记录或 Python traceback）、unknown。
- 新增可选 OutcomeSource、ExitCode，仅保存分类来源及可取得的退出码，不保存额外命令正文、结果或凭据。无法识别、混合/无效格式、任意工具或图片等保持 unknown，远程 MCP 使用原有独立结果，不套客户端文本规则。
- 诊断分类与业务执行证据分开：`pendingTool.outcome()`、`receiptOutcome()`、压缩事实、未知操作保护及自动重试规则保持原样。退出码 0 不证明文件正确保存，异常文本也可能是读取日志的内容，因此单列警示状态，不把它作为恢复/重试授权。
- 迟到结果按原工具身份使用相同诊断分类。门户耗时详情仍不逐条展示 AGY 阶段与工具回执。历史记录不批量回填，新分类用于更新后产生的观测事件。

## 验证与发布

- 全量 `go test ./...`、`go vet ./...`、差异检查通过；回执分类、结果不变、无正文泄漏、重复回执去重、迟到结果、原生/远程隔离及原自动恢复核心回归连续 5 次通过。补强测试确认文本退出状态不成为压缩执行证明，也不解除未知操作保护，不新增完整会话快照。
- 实现 **d24ceaae4af9d69a0828f52074091d0d77d19112** 已推送 main。提交后 Linux amd64/CGO=0/trimpath 构建使用 `-ldflags '-s -w'`，程序 **19,542,176 bytes**，SHA256 **60fc24c66f61b2aa256cc7e286fcfe66c62ef9cc8f2958a65976fbdfd1c93552**；构建 revision 对应该实现提交，无关未跟踪 ops/__pycache__/ 导致 modified=true，未纳入提交。
- 产物：`C:\Users\lly77\AppData\Local\Temp\codex-agy-receipt-release-52315beb6d0d49d996ce41b2ab392f59\portal-linux-amd64`。压缩包 6,100,032 bytes，上传后同时核对压缩包和解压程序的长度、完整 SHA256，再进入替换流程。
- AGY 无活跃/等待任务、常驻会话为 0；门户到 CPA 连续 **27.000 秒**无 TCP 数据或连接活动，最终复核后原子替换，仅重建门户，未创建备份或回滚副本。
- 门户容器 **212318be30ec** 于 **2026-11-16 12:25:42.180 +0800** 启动；**12:26:13** 验收 healthy，实际 portal 进程与挂载程序哈希相同。服务器本地、门户公网、API 公网 `/healthz` 均 200，客户端两处公网检查也均 200；启动 ERROR/panic/fatal 计数 0。
- 会话缓存仍为 late-client-result-routes-v6，磁盘会话 294、ready 会话 55，无 cache_error；AGY not_connected/sessions=0 为惰性状态。未发起真实模型、客户端工具或远端业务验收，实际回执分类待自然请求观察。
- CPA **6a99f7e729f7**（2026-10-22T05:50:39.056779009Z）、CPAMP **688354d0aead**（2026-11-14T19:01:36.313887916Z）与部署前 ID/启动时间相同，没有重启。CLI SHA256 仍为 **8b4fc29f938cab08c47e7471238184fe77a6c9757e0d640bf980f133998a6f7c**；目录刷新实验 false。客户端、预算及完整会话保存时机未改变。
- 远端临时压缩包、部署与验收助手已删除，暂存新程序已原子替换消失。发布信息只记交接，不添加到开源项目 README。
