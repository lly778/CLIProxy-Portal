# AGY 无推理协议核查（2026-11-14）

核查对象为服务器当前 CLI 1.5.3，SHA256 `8b4fc29f938cab08c47e7471238184fe77a6c9757e0d640bf980f133998a6f7c`。这是独立测试进程的结果，不是线上门户已部署新代码的证据。

## 方法与隔离

- 使用 `ops/agy/probe_protocol.py`，仅允许发送 initialize、session/new、session/load、session/close；代码不包含 session/prompt 调用。
- 每次创建独立临时 HOME、配置、工作目录、原生数据库和回环假 MCP 服务。没有接入原生产会话。
- 未登录模式只能握手，会话创建被认证拒绝；之后在独立 Linux mount namespace 内，把现有登录文件只读绑定到临时目录。没有复制、打印或修改凭据，没有重新登录。
- MCP 只提供 probe_alpha/probe_beta 两个假工具，执行入口始终拒绝。实际 tools/call 为零，推理提示为零。
- 退出先停止该测试进程组，再卸载临时只读绑定并清理测试目录。不重启门户、CPA、CPAMP 或生产 AGY 工作进程。

## 结果

| 检查 | 实测结果 | 允许得出的结论 |
| --- | --- | --- |
| ACP initialize | protocolVersion=1，agentInfo 版本 1.5.3 | ACP 握手成功 |
| 原生恢复声明 | loadSession=true；会话能力含 load、resume、close、list | 这些是运行时声明，真实恢复仍需验证原记录 |
| MCP 传输声明 | mcpCapabilities.http=true | 确认 HTTP；没有据此推断 MCP-over-ACP 或旧式 SSE transport |
| 新建空会话 | 成功，独立原生数据库存在 | 无推理也能完成会话设置与 MCP 握手 |
| 加载已关闭的空会话 | 成功，MCP 重新取目录 | session/load 会重新设置 MCP；不能当作目录通知生效 |
| 加载从未存在的 ID | 返回 result，sessionId 等于请求 ID，原生数据库不存在 | 单凭 session/load 返回成功，无法证明旧历史已恢复 |
| 动态目录通知 | 假服务声明 listChanged 并发出通知，20 秒内未观察到因通知而重新 tools/list | 支持状态保持 unknown；不能默认启用原生动态刷新 |
| 恢复附近通知 | 观察到 available_commands_update，外层仅 sessionId/update，未见可靠回合身份 | 仅验证设置阶段的通知形状，不能证明生成阶段一定没有扩展身份 |

最终 stateless 探测计数为 `lists=[0,1,1]`、`list_phases=[new,load,load]`、`catalog_refetched_before_load=false`；GET 请求 6 次、通知流 1 条、通知 1 次、工具执行 0 次。stateless 与门户默认传输一致，只保留一条活跃 GET；另一个 stateful 夹具为每次 initialize 发独立 session ID，每个 session 至多一条通知流，不再复用固定 session ID。stateful 变体未建立通知流，不能据此认定 CLI 永久不支持。

第一次数值增量曾被误判为目录通知生效；增加阶段归因后更正，最终代码未把它标为已支持。标准 message 事件及显式 `params={}` 均已尝试；最终合规夹具仍未观察到通知阶段重拉。未来可能存在延迟/惰性刷新，需要新的证据；本报告没有将其判定为永久不支持。

## 已落地的保护

1. 对这个已核查的精确程序哈希，原生恢复前以只读方式检查原数据库和 steps 游标。缺失文件或空 steps 明确为原生历史缺失，不调用可能返回假成功的 session/load。完整已完成的桥接历史满足原有条件时，才允许一次受限重建；Codex 原会话摘要和有未决工具的路径不会偷偷换会话。
2. 损坏、不可读或无法确认的数据库保留旧恢复记录并报错，不推断为会话丢失；session/load 返回不同 native ID 也拒绝接受。
3. 运行时证据绑定 SHA256，新 CLI 即便版本字符串相同也不继承这些特殊行为结论。
4. 工具事实回查入口加入原生 permission 白名单，同时继续检查当前 none、loading、checkpointing 状态，避免“有原始结果却被原生权限层拦住”。
5. 目录刷新候选通道支持认证 GET/SSE、有界连接、版本合并、单主通知与备用接管、通知/获取分别记录；同一通知不广播到多个连接。更新与撤销立即按当前契约生效，挂起动作与原结果保持原身份。陈旧目录响应不能确认新版，也不允许断开通知流取消真实任务；提供 Origin 时校验同源回环地址。

## 默认配置与剩余工作

`AGY_CATALOG_REFRESH_EXPERIMENTAL=false` 是默认值；Compose 和示例配置都保留 false。只有明确开启诊断开关且 CLI 哈希匹配，才暴露候选通知通道；其运行时支持状态仍是 unknown。没有启用新的外部执行器。

D3 的内部代次、loading/idle 隔离已存在，但原生生成通知若没有可靠回合身份，且在新回合开始后才迟到，仍无法完全归属。当前无推理探测不能消除这项边界。

第五批另补了受限远程 MCP broker 的本地实现及假服务验收，详见 [优化清单](OPTIMIZATION-CHECKLIST-2026-11-14.md)。它与本报告的 CLI 目录刷新探测是两项独立工作，不代表通知刷新已可用，也没有做真实远端/客户端验收。托管 file_search/code_interpreter 和 CFG 约束采样仍未实现。

探测临时助手已清理，所属探测 CLI 进程为零；生产进程和配置未变更。本次握手证据不能用于宣称真实远端执行或生成阶段回合身份已经验证。

## 第六批的 initialize-only 核查

脚本新增 `--initialize-only`，在 initialize 后退出，不发送 new/load/close 或 prompt。本次独立 HOME 探测未使用登录文件，精确 CLI SHA256 未变；结果为 ACP=1、Antigravity ACP Server 1.5.3、`mcpCapabilities={"http":true}`。未出现 `mcpCapabilities.acp`，因此 MCP-over-ACP 保持 unknown；没有把未声明改写成 unsupported。

门户现在识别标准的显式布尔 acp 声明，来源记为 acp_declaration，仍不启用新传输。未知 MCP-over-ACP 方法以固定不可用错误结束请求，不影响读取连接。这个扩展及声明位置可参考 [官方 Rust SDK](https://agentclientprotocol.github.io/rust-sdk/mcp-over-acp.html)；不能因 SDK 提供此能力就推断 AGY 已支持。

临时探测脚本已删除，所属探测 CLI 进程为零。没有生产会话、凭据或服务配置变更。C2 的通知刷新结论未因本次初始化而改变。
