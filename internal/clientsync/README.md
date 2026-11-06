# Codex 配置同步

管理员在 **上游管理 → 客户端模型同步** 点击“配置模型与思考强度”。弹窗左侧搜索、勾选完整模型目录，右侧设置每个模型的默认思考强度并指定默认模型，点击弹窗内的“保存配置”直接保存并生效，取消不改变原配置。主页面仅保留下载和配置入口；所有屏幕只用模型标签展示每个已保存模型的默认思考强度，并标出默认模型，不重复显示上方统计。

保存即可获取，无需发布开关、门户手机号或密码。尚未设置模型时接口返回 404。配置是公开客户端默认值，不包含 API Key 或用户身份，也不改变 CPA 的调用鉴权、路由、模型别名或思考强度上限。

## 备份与恢复

已保存的可选模型、默认模型、各模型默认思考强度及配置修订号存放在门户 `portal.db` 的 `settings` 表中，随一致性数据库快照备份；普通恢复与重建并恢复都会还原，无需单独添加备份来源。调用地址在获取时读取当前部署的 `CPA_API_BASE_URL`，支持档位读取当前上游目录，不把旧站点地址或过期能力缓存写回新部署。

电脑本地的 `config.toml`、`portal-models.json`、API Key 和 `.portal-sync-*.bak` 不属于门户服务器备份。恢复门户后可重新下载脚本同步；本机已有有效 Key 可以沿用。脚本在本机应用时创建的回退副本用于恢复本机文件，与门户 `.cbackup` 是两套独立备份。

## 运行

从门户下载单个 `Sync-Codex.bat`，双击即可运行，地址已内置，无需解压、安装依赖、填写 PS1 路径或修改文件关联。批处理使用系统自带的 Windows PowerShell 5.1，将内置脚本释放到独立临时目录，运行结束后删除临时脚本和目录；不会另外下载可执行脚本，也不修改持久执行策略。生成和释放时均会清理重复 BOM，并以单个 UTF-8 BOM 写入，中文提示兼容 Windows PowerShell 5.1。启动前先检查 PowerShell 语法；准备或语法检查失败会显示错误并暂停，不直接闪退。

也可以从 PowerShell 或命令提示符带参数运行：

```powershell
# 下载的文件不需要填写门户地址；无参数进入交互预览和应用确认。
.\Sync-Codex.bat

# 自动应用；仍须交互输入尚未配置的 API Key。
.\Sync-Codex.bat -Apply

# 可选择替换已有的调用 Key。
.\Sync-Codex.bat -UpdateApiKey -Apply

# 指定独立 Codex 配置目录；含空格的路径加双引号。
.\Sync-Codex.bat -CodexHome "C:\Codex Instance\.codex" -Apply
```

无参数运行时先展示变更，最后确认处回车即可备份并应用，输入 `n` 取消；“是否更新 Key”仍默认沿用，只有输入 `y` 才提示换 Key。窗口等待回车后关闭。带参数运行未指定 `-Interactive` 或 `-Apply` 时仍仅预览。应用前完全退出 Codex；不会强制结束进程。文件权限通过 .NET 文件接口只写访问规则，不修改审计规则，不要求 `SeSecurityPrivilege`。

仓库中的 `Sync-Codex.bat` 是下载生成器使用的启动模板，完整批处理文件由服务器嵌入脚本和站点地址后生成。原 `Sync-Codex.ps1` 作为同步逻辑源文件保留，Windows PowerShell 5.1 和 PowerShell 7 均可运行；开发时可显式传入 `-PortalUrl https://portal.example.com -Interactive`。

脚本统一直接配置 Codex，没有模式、供应商列表或序号选择，也不读取其他配置管理器的数据。默认目录为 `$CODEX_HOME`，未设置时使用 `%USERPROFILE%\.codex`；`-CodexHome` 可显式覆盖。

## 实际修改

| 文件 | 修改 |
| --- | --- |
| `config.toml` | 设置根级 `model`、`model_reasoning_effort`、`model_catalog_json` 和 `model_provider`；使用固定的 `model_providers.cliproxy_portal` 连接当前门户提供的 Responses API 地址 |
| `portal-models.json` | 生成 Codex 启动时实际读取的模型目录，仅包含管理员选择的模型及各自默认思考强度 |

这同时更新生效配置和模型目录，不只是保存一份供应商列表。默认模型排在目录首位，每个模型均设为可见。匹配到本机已有模型目录或 `models_cache.json` 的名称时保留其元数据；没有精确匹配的自定义别名采用基础描述、保守工具能力和 128K 窗口，需要其他能力时可在本机目录补充后再次同步。

所有模型的支持档位与默认强度分开处理。门户在现有完整目录缓存中保留上游明确声明的完整档位，OAuth 别名、自定义 API 别名和配置前缀继承源模型的声明。脚本只用上游返回的 `supported_reasoning_levels`，不从本机模型缓存、已有目录、预算区间或默认值合并、推测档位。选择“沿用模型默认”也保留上游声明的全部档位；更改默认只设置 `default_reasoning_level`。默认值不在声明档位中时停止并提示刷新或调整；上游未声明档位时列表为空。

配置的 OpenAI 兼容上游还会由服务器读取其 `/models` 元数据，识别 `effort.supported_levels`（如 DeepSeek）、`thinking.levels` 和 `supported_reasoning_levels`。只使用对应上游配置中的凭据，不向上游发送 CPAMP 管理员 Key，不跟随重定向，不返回凭据；结果仍纳入同一个 1 小时目录缓存与刷新机制。这里只读取模型目录，不发起推理请求。

“沿用模型默认”不写入顶层思考强度覆盖，目录中的默认值设为 `null`。脚本设置的 `model_catalog_json` 是独立文件，不替换 `models_cache.json`；本机目录仅作为指令、上下文等非档位元数据的来源。

每个模型都会补齐 Codex 所需的 `base_instructions`：已有指令保留，只有 `model_messages.instructions_template` 时镜像其内容，均无时使用通用编程助手指令。旧脚本生成的缺失字段条目会在重新同步时补齐。写入前检查模型数量、唯一名称、指令、基本字段类型及思考档位，并对序列化后的完整目录再次检查；未通过则不替换文件。

脚本会读取 Codex 当前活动连接（包括根级 `profile` 选择的连接）的 `experimental_bearer_token` 或有效 `env_key`，其次读取脚本原有连接，最后读取 `auth.json` 中的 `OPENAI_API_KEY`；环境变量支持当前进程、用户和系统作用域。找到 Key 后默认沿用，也可选择替换；都没有时必须隐藏输入 Key。ChatGPT 登录的 OAuth access/refresh token 不会用作 API Key。输入的新 Key 写入自有 provider 的 `experimental_bearer_token`，不回显；配置和备份限制为当前用户、SYSTEM、管理员访问。已有 `env_key` 可继续使用，避免再次保存明文。Key 不从门户导出，也不自动领取。`auth.json` 仅读取，聊天历史、会话数据库、安装文件及其他 provider 的配置表均不修改。

脚本保留其他 TOML 设置，只替换目标根字段和自有 provider 表。根级 `profile` 选择会移除，使默认设置直接生效，已有 profile 表保留。显式启动参数、项目配置、其他 profile 或组织管理策略仍可能覆盖用户默认设置。暂不自动处理多行 TOML 字符串、目标内联表及复杂自有 provider 子表，遇到这些结构会在写入前停止。

应用时先校验配置未被其他进程更改，准备两个文件，再依次原子替换。已有文件生成 `.portal-sync-日期-随机值.bak`，中途失败会恢复本次已经替换的文件；若发现文件又被外部程序修改，则保留备份并提示手动恢复。备份包含原连接凭据，须与配置同等保管。恢复时退出 Codex，用对应备份还原两个文件；本次新建的目录文件没有旧备份时可移除该文件。

应用后完全重启 Codex 并新建聊天，加载新的模型目录和默认值。已经手动选择模型或思考强度的已有聊天保留其会话选择。不要让其他配置管理器随后覆盖这两个文件。

## 接口与目录缓存

- `GET /downloads/codex-sync.bat`：页面的默认下载入口，公开下载单个可双击运行的批处理文件，内置同步脚本和服务器配置的公开门户地址，不含登录信息或 API Key。
- `GET /downloads/codex-sync.ps1`：保留旧 PowerShell 下载地址，供已有链接兼容使用；页面默认提供批处理格式。
- `GET /api/client-model-config`：公开读取已保存的模型默认配置、上游声明的 `supported_reasoning_levels` 与当前 `CPA_API_BASE_URL`，无需账号或 Key；每 IP 每分钟最多 60 次，`Cache-Control: no-store`。档位复用完整目录的 1 小时缓存、并发合并及失效规则；管理服务临时不可用时仍返回已保存的默认配置，但不回退到本机档位。无配置返回 404，读取失败返回 500，不调用任何模型。
- 管理员的保存接口仍要求管理员角色及 CSRF，版本冲突返回 409；保存写入系统日志。不存在单独发布或暂停获取的开关。
- `GET /admin/upstreams/client-models/catalog`：仅管理员可取完整目录，打开编辑弹窗时按需读取。包含停用、冷却和暂不可用模型，管理员已选但上游后来移除的名称仍可保留。汇总缓存与 OAuth 模型定义保持 1 小时，并发查询合并，修改后按同一版本失效。需要立即重新读取时使用上游管理顶部“刷新配置”；弹窗不另设“重新获取”按钮。

实现依据：[OpenAI 配置参考](https://developers.openai.com/codex/config-reference)、[高级配置](https://developers.openai.com/codex/config-advanced)。支持情况取决于本机 Codex 版本；脚本不通过发起推理请求验证配置。
