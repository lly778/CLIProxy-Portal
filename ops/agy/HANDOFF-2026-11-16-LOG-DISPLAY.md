# 请求日志耗时展示调整（2026-11-16）

用户要求门户日志的耗时详情不再具体列示 AGY 阶段与工具回执。删除 requestView 中的观测记录查询及追加显示，清理不再使用的展示函数和对应测试。保留原服务器总耗时、分段耗时及模型调用耗时；后台 AGY 观测采集、保存、用量与恢复逻辑保持原样。

## 验证与发布

- `go test ./internal/httpserver ./internal/store`、`go vet ./internal/httpserver`、差异检查及 Linux amd64/CGO=0/trimpath 构建通过。
- 实现提交 **3b1ad80cc43bd94fc376935ce8407d55d0a562d3** 已推送 main。发布程序为 `C:\Users\lly77\AppData\Local\Temp\codex-portal-timing-release-6e8b0e5821be4ad79849c3a0bc97196e\portal-linux-amd64`，27,974,021 bytes，SHA256 **8bf9549d370b481648adde378c8a9e102c1c04f98c717dac1642724a1abb55bd**。构建元数据 revision 对应该实现提交；无关未跟踪 ops/__pycache__/ 导致 modified=true，未纳入提交。
- 上传较慢且中途断开；核对已上传前缀的哈希后续传压缩包，完整解压并确认长度与 SHA256 一致才进入发布。临时空闲检测脚本出现字节字面量转义问题，修正后重新检查；该错误发生在替换前，没有影响在线程序。
- 部署前 AGY 无生成或等待工具任务、常驻会话为 0，磁盘会话 294。门户至 CPA 连续 **27.000 秒**无 TCP 数据或连接活动，最终复核后原子替换程序，仅重建门户，未创建备份或回滚副本。
- **2026-11-16 11:52:29.864 +0800** 新门户容器 **fe922f1ad835** 启动；**11:53:02** 验收 healthy，实际 portal 进程与挂载程序的 SHA256 一致。服务器本地、门户公网、API 公网 `/healthz` 均为 200，客户端访问两处公网也均为 200；启动 ERROR/panic/fatal 计数 0。
- 缓存版本仍为 **late-client-result-routes-v6**，磁盘会话仍为 **294**，ready 会话 55，无 cache_error。AGY not_connected/sessions=0 是重启后的惰性状态，没有为验收发起真实模型、客户端或远程工具调用。
- CPA **6a99f7e729f7**（2026-10-22T05:50:39.056779009Z）、CPAMP **688354d0aead**（2026-11-14T19:01:36.313887916Z）与部署前 ID、启动时间相同，没有重启。CLI SHA256 仍为 **8b4fc29f938cab08c47e7471238184fe77a6c9757e0d640bf980f133998a6f7c**；目录刷新实验保持 false。客户端和完整会话保存时机未改变。
- 远端临时压缩包及部署、验收助手已清理，暂存新程序已原子替换消失。发布记录仅写入本交接文档，不写入面向开源用户的 README。
