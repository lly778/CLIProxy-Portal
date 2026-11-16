package agy

// Public, bounded descriptions for local input errors. The tracker and request
// log must retain these codes without copying private instructions, URLs, tool
// arguments or arbitrary upstream error messages into their summaries.
var validationFailures = map[string]struct {
	status  int
	message string
}{
	"invalid_request":               {400, "AGY 请求格式无效"},
	"missing_input":                 {400, "缺少输入消息"},
	"invalid_input":                 {400, "AGY 输入必须是文字或消息列表"},
	"invalid_instructions":          {400, "instructions 必须为文本"},
	"unsupported_parameter":         {400, "AGY 不支持本次请求中的参数"},
	"unsupported_input_item":        {400, "AGY 不支持该上下文项目类型"},
	"opaque_context_unsupported":    {400, "AGY 无法读取其他供应商的加密或不透明压缩上下文"},
	"invalid_role":                  {400, "AGY 消息角色无效"},
	"invalid_content":               {400, "AGY 消息内容格式无效"},
	"unsupported_content":           {400, "AGY 不支持该消息内容类型"},
	"image_url_unsupported":         {400, "AGY 图片需使用内嵌图片数据，不支持远程地址或文件引用"},
	"invalid_image":                 {400, "图片编码、数据或格式无效"},
	"invalid_document":              {400, "输入文档格式无效"},
	"unsupported_document":          {400, "AGY 不支持该输入文档类型"},
	"text_projection_unsupported":   {400, "AGY 无法完整转换该工具结果或上下文"},
	"invalid_tool_result":           {400, "客户端工具结果格式无效"},
	"invalid_native_tool_result":    {400, "原生客户端工具结果格式无效"},
	"invalid_tool_followup":         {400, "工具结果之后只能追加用户消息或指令"},
	"invalid_tool_search_result":    {400, "客户端工具发现结果无效"},
	"invalid_tool_reference":        {400, "客户端工具引用无效或尚未加载"},
	"unsupported_tool":              {400, "AGY 不支持该工具声明"},
	"invalid_tool_name":             {400, "工具名称无效"},
	"duplicate_tool":                {400, "工具名称重复"},
	"invalid_tool_namespace":        {400, "工具命名空间声明无效"},
	"invalid_native_tool":           {400, "原生客户端工具声明无效"},
	"unsupported_shell_environment": {400, "AGY 本机 Shell 需要客户端声明本机执行环境"},
	"invalid_computer_display":      {400, "客户端电脑操作的显示尺寸无效"},
	"invalid_tool_choice":           {400, "工具选择策略无效或与当前工具目录不匹配"},
	"too_many_tools":                {400, "工具数量超过 AGY 接入限制"},
	"duplicate_result":              {400, "同一批次重复提交了工具结果"},
	"unsupported_model_option":      {400, "AGY 不支持所请求的模型选项"},
	"request_too_large":             {413, "AGY 请求超过大小限制"},
	"context_too_large":             {413, "AGY 上下文超过本次输入安全限制"},
	"latest_input_too_large":        {413, "最新输入超过安全限制，请拆分后重试"},
	"too_many_images":               {413, "图片数量超过 AGY 接入限制"},
	"tool_definitions_too_large":    {413, "工具定义超过 AGY 接入限制"},
	"tool_update_too_large":         {413, "工具定义更新与新增输入合计超过安全限制"},
}
