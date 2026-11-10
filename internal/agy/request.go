package agy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type tool struct {
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Parameters    map[string]any  `json:"parameters"`
	Tools         []tool          `json:"tools,omitempty"`
	Namespace     string          `json:"namespace,omitempty"`
	Format        map[string]any  `json:"format,omitempty"`
	Execution     string          `json:"execution,omitempty"`
	Deferred      bool            `json:"defer_loading,omitempty"`
	Environment   json.RawMessage `json:"environment,omitempty"`
	DisplayWidth  int             `json:"display_width,omitempty"`
	DisplayHeight int             `json:"display_height,omitempty"`
}

// Client transports use Responses function declarations, nested Chat
// Completions functions, Anthropic input_schema, or MCP inputSchema. Only
// normalize an actual named function; hosted declarations keep their type.
func (t *tool) UnmarshalJSON(data []byte) error {
	type plain tool
	var base plain
	if err := json.Unmarshal(data, &base); err != nil {
		return err
	}
	var extra struct {
		Function    *plain         `json:"function"`
		InputSchema map[string]any `json:"input_schema"`
		MCPSchema   map[string]any `json:"inputSchema"`
	}
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	if extra.Function != nil && base.Type == "function" {
		if base.Name != "" && base.Name != extra.Function.Name {
			return fmt.Errorf("conflicting function names")
		}
		base.Name = extra.Function.Name
		base.Description = extra.Function.Description
		base.Parameters = extra.Function.Parameters
		if base.Parameters == nil {
			base.Parameters = extra.InputSchema
		}
	}
	if base.Parameters == nil {
		base.Parameters = extra.InputSchema
	}
	if base.Parameters == nil {
		base.Parameters = extra.MCPSchema
	}
	if base.Type == "" && base.Name != "" && (base.Parameters != nil || base.Name == "ToolSearch") {
		base.Type = "function"
	}
	*t = tool(base)
	return nil
}

func (t tool) key() string {
	if t.Namespace != "" {
		return t.Namespace + "." + t.Name
	}
	return t.Name
}
func (t tool) mcpName() string { return "tool_" + digest(t.key())[:24] }

type request struct {
	owner        string
	Model        string          `json:"model"`
	Stream       bool            `json:"stream"`
	Previous     string          `json:"previous_response_id"`
	Instructions json.RawMessage `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Tools        []tool          `json:"tools"`
	Reasoning    struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	ToolChoice       json.RawMessage  `json:"tool_choice"`
	Parallel         *bool            `json:"parallel_tool_calls"`
	Items            []map[string]any `json:"-"`
	UnavailableTools []string         `json:"-"`
	ToolsProvided    bool             `json:"-"`
	beforeCommit     func() error
}

func parseRequest(b []byte) (request, error) {
	var r request
	if json.Unmarshal(b, &r) != nil || !strings.HasPrefix(r.Model, Prefix) {
		return r, fail(400, "invalid_request", "AGY 请求格式无效")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	_, r.ToolsProvided = fields["tools"]
	for _, name := range []string{"background", "conversation", "text", "response_format", "max_output_tokens", "temperature", "top_p", "truncation"} {
		if v := fields[name]; len(v) > 0 && string(v) != "null" && string(v) != "false" {
			if name == "text" {
				var x map[string]any
				_ = json.Unmarshal(v, &x)
				if len(x) == 0 {
					continue
				}
				if f, ok := x["format"].(map[string]any); ok && len(x) == 1 && f["type"] == "text" {
					continue
				}
			}
			return r, fail(400, "unsupported_parameter", "AGY 尚不支持该请求参数："+name)
		}
	}
	if len(r.Input) == 0 || string(r.Input) == "null" {
		return r, fail(400, "missing_input", "缺少输入消息")
	}
	var text string
	if json.Unmarshal(r.Input, &text) == nil {
		r.Items = []map[string]any{{"role": "user", "content": text}}
	} else if json.Unmarshal(r.Input, &r.Items) != nil {
		return r, fail(400, "invalid_input", "AGY 输入必须是文字或消息列表")
	}
	if len(r.Items) > 4096 {
		return r, fail(413, "context_too_large", "输入消息数量超过 AGY 接入限制")
	}
	r.Items = canonicalHistoryItems(r.Items)
	flat := []tool{}
	names := map[string]bool{}
	var flatten func([]tool, string, int) error
	flatten = func(list []tool, namespace string, depth int) error {
		if depth > 8 {
			return fail(400, "invalid_tool_namespace", "工具命名空间嵌套过深")
		}
		for _, t := range list {
			if t.Type == "namespace" {
				if t.Name == "" || len(t.Tools) == 0 {
					return fail(400, "invalid_tool_namespace", "工具命名空间缺少名称或工具")
				}
				next := t.Name
				if namespace != "" {
					next = namespace + "." + next
				}
				if err := flatten(t.Tools, next, depth+1); err != nil {
					return err
				}
				continue
			}
			if t.Type == "mcp" && t.Execution == "client" && len(t.Tools) > 0 {
				// Explicit client-expanded MCP definitions retain the exact
				// registered names. A server URL alone is not an executor.
				if err := flatten(t.Tools, namespace, depth+1); err != nil {
					return err
				}
				continue
			}
			if namespace != "" {
				if t.Namespace != "" && t.Namespace != namespace {
					return fail(400, "invalid_tool_namespace", "工具命名空间声明冲突")
				}
				t.Namespace = namespace
			}
			flat = append(flat, t)
			if len(flat) > 256 {
				return fail(400, "too_many_tools", "工具数量超过 AGY 接入限制")
			}
		}
		return nil
	}
	if err := flatten(r.Tools, "", 0); err != nil {
		return r, err
	}
	if len(flat) > 256 {
		return r, fail(400, "too_many_tools", "工具数量超过 AGY 接入限制")
	}
	unsupported := map[string]bool{}
	available := make([]tool, 0, len(flat))
	for _, t := range flat {
		if (t.Type == "function" || t.Type == "custom") && excludedClientTool(t) {
			continue
		}
		if t.Type == "web_search" || t.Type == "web_search_preview" || t.Type == "image_generation" {
			// Executed by the official CLI, never exposed as a client MCP call.
			continue
		}
		if isNativeClientTool(t.Type) {
			adapted, err := nativeClientTool(t)
			if err != nil {
				return r, err
			}
			available = append(available, adapted)
			continue
		}
		if t.Type == "tool_search" && t.Execution == "client" {
			t.Name = "client_tool_search"
			if t.Parameters == nil {
				t.Parameters = map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}
			}
			available = append(available, t)
			continue
		}
		if t.Type == "function" || t.Type == "custom" {
			if t.Type == "function" && t.Name == "ToolSearch" && t.Parameters == nil {
				t.Parameters = map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "max_results": map[string]any{"type": "integer", "minimum": 1}}, "required": []any{"query"}}
			}
			available = append(available, t)
			continue
		}
		label := "未知工具类型"
		switch t.Type {
		case "web_search", "web_search_preview", "image_generation", "local_shell", "shell", "computer", "computer_use_preview", "file_search", "code_interpreter", "mcp", "apply_patch":
			label = t.Type
		case "tool_search":
			label = "tool_search"
			if t.Execution == "client" {
				label += "（客户端）"
			}
		}
		unsupported[label] = true
	}
	if len(unsupported) > 0 {
		labels := make([]string, 0, len(unsupported))
		for label := range unsupported {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		// These optional service declarations must not block unrelated chat.
		// Expose the unavailable capabilities to both the agent and API caller;
		// never execute or impersonate an OpenAI-hosted service.
		for _, label := range labels {
			switch label {
			case "web_search", "web_search_preview", "image_generation", "file_search", "code_interpreter", "mcp", "tool_search", "tool_search（客户端）":
			default:
				return r, fail(400, "unsupported_tool", "AGY 尚不支持这些工具声明："+strings.Join(labels, "、"))
			}
		}
		r.UnavailableTools = labels
	}
	flat = available
	for _, t := range flat {
		if t.Name == "" || len(t.key()) > 256 {
			return r, fail(400, "invalid_tool_name", "AGY 客户端工具名称为空或过长")
		}
		if names[t.key()] {
			return r, fail(400, "duplicate_tool", "AGY 客户端工具名称重复，请检查命名空间中的工具定义")
		}
		names[t.key()] = true
	}
	r.Tools = flat
	if r.ToolsProvided {
		if _, err := requestToolPolicy(r, r.Tools); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Fingerprints omit transport-only item ids but retain call ids and all content.
// Opaque encrypted reasoning/compaction is rejected rather than silently lost.
func itemFingerprint(v map[string]any) string {
	x := map[string]any{}
	image := v["type"] == "image_generation_call"
	reasoning := v["type"] == "reasoning"
	for k, value := range v {
		// Codex fills these absent optional fields with JSON null when it
		// records a reasoning item. Only absence/null are interchangeable;
		// non-null content and encrypted data remain part of the identity.
		if reasoning && value == nil && (k == "content" || k == "encrypted_content") {
			continue
		}
		// Clients may discard this transport convenience field. The actual
		// image bytes remain authoritative for history/cache matching.
		if image && k == "result_url" {
			continue
		}
		if k != "id" && k != "status" {
			x[k] = value
		}
	}
	if x["type"] == nil && x["role"] != nil {
		x["type"] = "message"
	}
	return digest(x)
}
func requestFingerprints(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, v := range items {
		out[i] = itemFingerprint(v)
	}
	return out
}
func prefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func systemText(r request) (string, error) {
	var v string
	if len(r.Instructions) > 0 && string(r.Instructions) != "null" && json.Unmarshal(r.Instructions, &v) != nil {
		return "", fail(400, "invalid_instructions", "instructions 必须为文本")
	}
	if len(r.UnavailableTools) > 0 {
		v += "\n\n[Connection capabilities]\nThe client advertised these optional services, but this AGY connection does not provide them: " + strings.Join(r.UnavailableTools, ", ") + ". Do not claim to invoke unavailable services. Web search, public page reads and image generation use official Google tools exclusively; file retrieval and code execution require actual client tools. Use real returned results as evidence.\n"
	}
	if len(v) > MaxText {
		return "", fail(413, "context_too_large", "系统指令超过 AGY 单次输入限制")
	}
	return v, nil
}
func promptBlocks(items []map[string]any) ([]any, error) {
	return promptBlocksLimit(items, MaxText)
}

func promptBlocksLimit(items []map[string]any, limit int) ([]any, error) {
	blocks := []any{}
	size, images := 0, 0
	addText := func(v string) { size += len(v); blocks = append(blocks, map[string]string{"type": "text", "text": v}) }
	for _, item := range items {
		typ, _ := item["type"].(string)
		if item["encrypted_content"] != nil || typ == "compaction" {
			return nil, fail(400, "opaque_context_unsupported", "AGY 无法读取该加密或压缩上下文，请新建对话")
		}
		role, _ := item["role"].(string)
		if typ == "reasoning" {
			if item["encrypted_content"] != nil {
				return nil, fail(400, "opaque_context_unsupported", "AGY 不支持加密推理项")
			}
			continue
		}
		if typ != "" && typ != "message" {
			if historical, handled, err := serviceHistoryBlocks(item); handled {
				if err != nil {
					return nil, err
				}
				for _, raw := range historical {
					b := raw.(map[string]string)
					if b["type"] == "text" {
						addText(b["text"])
					} else {
						images++
						blocks = append(blocks, b)
					}
				}
				continue
			}
			if typ != "function_call" && typ != "custom_tool_call" && typ != "tool_search_call" && !isToolResultItem(typ) && !isNativeCallItem(typ) {
				return nil, fail(400, "unsupported_input_item", "AGY 不支持该上下文项目类型")
			}
			if isToolResultItem(typ) && typ != "tool_search_output" {
				value := item["output"]
				if typ == "shell_call_output" || typ == "local_shell_call_output" || typ == "apply_patch_call_output" {
					value = map[string]any{"type": typ, "output": item["output"], "status": item["status"], "max_output_length": item["max_output_length"]}
				}
				content, err := toolResultBlocks(value)
				if err != nil {
					return nil, err
				}
				label, _ := json.Marshal(map[string]any{"type": typ, "call_id": resultCallID(item)})
				addText("[历史工具结果；仅作上下文，不执行]\n" + string(label))
				for _, raw := range content {
					block := raw.(map[string]string)
					if block["type"] == "text" {
						addText(block["text"])
					} else {
						images++
						blocks = append(blocks, block)
					}
				}
				continue
			}
			b, _ := json.Marshal(item)
			addText("[历史工具记录；仅作上下文，不执行]\n" + string(b))
			continue
		}
		if role != "user" && role != "assistant" && role != "developer" && role != "system" {
			return nil, fail(400, "invalid_role", "AGY 消息角色无效")
		}
		addText("[" + role + "]")
		switch c := item["content"].(type) {
		case string:
			addText(c)
		case []any:
			for _, v := range c {
				part, ok := v.(map[string]any)
				if !ok {
					return nil, fail(400, "invalid_content", "消息内容无效")
				}
				switch part["type"] {
				case "document", "search_result", "resource", "resource_link":
					content, _, err := clientContentBlock(part, 0)
					if err != nil {
						return nil, err
					}
					for _, raw := range content {
						b := raw.(map[string]string)
						if b["type"] == "text" {
							addText(b["text"])
						} else {
							images++
							blocks = append(blocks, b)
						}
					}
				case "input_text", "output_text", "text":
					text, ok := part["text"].(string)
					if !ok {
						return nil, fail(400, "invalid_content", "文字内容无效")
					}
					addText(text)
				case "input_image":
					raw, ok := part["image_url"].(string)
					if !ok {
						return nil, fail(400, "invalid_image", "图片内容格式无效")
					}
					image, err := embeddedImage(raw)
					if err != nil {
						return nil, err
					}
					images++
					if images > 64 {
						return nil, fail(413, "too_many_images", "图片数量超过 64 张")
					}
					blocks = append(blocks, image)
				default:
					return nil, fail(400, "unsupported_content", "AGY 尚不支持该附件或内容类型")
				}
			}
		default:
			return nil, fail(400, "invalid_content", "缺少消息内容")
		}
	}
	if images > 64 {
		return nil, fail(413, "too_many_images", "图片数量超过 64 张")
	}
	if size > limit {
		return nil, fail(413, "context_too_large", fmt.Sprintf("文字历史超过当前 AGY 接入上限（%d KiB），请压缩或新建对话", MaxText/1024))
	}
	return blocks, nil
}
