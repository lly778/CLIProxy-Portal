package agy

import (
	"encoding/json"
	"sort"
	"strings"
)

const callToolName = "portal_call_tool"

func sessionToolDelta(s *session, next []tool) (string, string, error) {
	hash := digest(next)
	if hash == s.toolHash {
		return "", hash, nil
	}
	text, err := toolDelta(s.tools, next)
	return text, hash, err
}

func eagerTools(tools []tool) []tool {
	out := []tool{}
	for _, t := range tools {
		if !t.Deferred && !excludedClientTool(t) {
			out = append(out, t)
		}
	}
	return out
}

// A fresh session may replay a complete, client-supplied Responses history.
// Restore only discoveries paired with an earlier client search call in that
// history; an isolated or foreign output cannot activate a tool.
func historicalLoadedTools(r request) ([]tool, error) {
	search := false
	for _, t := range r.Tools {
		search = search || isClientSearch(t)
	}
	if !search {
		return nil, nil
	}
	calls := map[string]bool{}
	loaded := []tool{}
	for _, item := range r.Items {
		id, _ := item["call_id"].(string)
		if item["type"] == "tool_search_call" && item["execution"] == "client" && id != "" {
			calls[id] = true
		}
		if item["type"] == "function_call" && item["name"] == "ToolSearch" && id != "" {
			calls[id] = true
		}
		if item["type"] == "function_call_output" && calls[id] {
			tools, err := referencedTools(item["output"], r.Tools)
			if err != nil {
				return nil, err
			}
			loaded = mergeTools(tools, loaded)
			delete(calls, id)
		}
		if item["type"] == "tool_search_output" && item["execution"] == "client" && calls[id] {
			if _, ok := item["tools"].([]any); !ok {
				return nil, fail(400, "invalid_tool_search_result", "历史工具发现结果缺少定义数组")
			}
			tools, err := parseLoadedTools(item["tools"])
			if err != nil {
				return nil, err
			}
			loaded = mergeTools(tools, loaded)
			delete(calls, id)
		}
	}
	if len(loaded) > 256 {
		return nil, fail(400, "too_many_tools", "当前已加载工具数量超过 256 个")
	}
	return loaded, nil
}

func mergeTools(base, loaded []tool) []tool {
	out := append([]tool{}, filterClientTools(base)...)
	seen := map[string]bool{}
	for _, t := range out {
		seen[t.key()] = true
	}
	for _, t := range loaded {
		if !excludedClientTool(t) && !seen[t.key()] {
			out = append(out, t)
			seen[t.key()] = true
		}
	}
	return out
}

// Called under the session lock. Explicit lists replace the active base;
// omitted tools preserve it when continuing an existing response chain.
func requestedTools(s *session, r request) ([]tool, []tool) {
	if !r.ToolsProvided {
		return filterClientTools(s.tools), filterClientTools(s.loadedTools)
	}
	base := eagerTools(r.Tools)
	search := false
	for _, t := range base {
		search = search || isClientSearch(t)
	}
	if search {
		return mergeTools(base, s.loadedTools), append([]tool{}, s.loadedTools...)
	}
	return base, []tool{}
}

// Keep the initial MCP list stable. Changed definitions are delivered once as
// context, and dispatched through a fixed helper against the live allowlist.
func toolDelta(old, next []tool) (string, error) {
	previous := map[string]string{}
	for _, t := range old {
		previous[t.key()] = digest(t)
	}
	lines := []string{}
	for _, t := range next {
		if previous[t.key()] != digest(t) {
			description := map[string]any{"name": t.key(), "type": t.Type, "description": t.Description, "parameters": t.Parameters}
			if t.Type == "custom" {
				description["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}}
				description["format"] = t.Format
			}
			encoded, _ := json.Marshal(description)
			lines = append(lines, "Available via "+callToolName+" with its exact name and arguments object: "+string(encoded))
		}
		delete(previous, t.key())
	}
	removed := make([]string, 0, len(previous))
	for name := range previous {
		removed = append(removed, name)
	}
	sort.Strings(removed)
	for _, name := range removed {
		encoded, _ := json.Marshal(name)
		lines = append(lines, "Disabled tool: "+string(encoded)+". Do not call it, including via its old direct declaration.")
	}
	if digest(clientCapabilities(old)) != digest(clientCapabilities(next)) {
		if text := clientCapabilityText(next); text != "" {
			lines = append(lines, text)
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	text := "[Client tool configuration update]\n" + strings.Join(lines, "\n") + "\n[End client tool configuration update]"
	if len(text) > MaxText/2 {
		return "", fail(413, "tool_update_too_large", "本轮工具定义更新过大，请分批更新工具")
	}
	return text, nil
}

func parseLoadedTools(value any) ([]tool, error) {
	encoded, err := json.Marshal(map[string]any{"model": "agy/schema-validation", "input": "validate", "tools": value})
	if err != nil {
		return nil, fail(400, "invalid_tool_search_result", "工具发现返回的定义格式无效")
	}
	r, err := parseRequest(encoded)
	if err != nil || len(r.UnavailableTools) != 0 {
		return nil, fail(400, "invalid_tool_search_result", "工具发现只能返回有效的客户端函数或自定义工具")
	}
	for i := range r.Tools {
		if r.Tools[i].Type != "function" && r.Tools[i].Type != "custom" {
			return nil, fail(400, "invalid_tool_search_result", "工具发现不能嵌套其他工具发现或托管服务")
		}
		r.Tools[i].Deferred = false
	}
	return r.Tools, nil
}

// All callers hold s.mu. Names and arguments remain validated against the
// current client declarations even when AGY still advertises an older schema.
func allowedToolCall(s *session, name string, args map[string]any) (tool, map[string]any, bool) {
	indirect := name == callToolName
	if indirect {
		name, _ = args["name"].(string)
		args, _ = args["arguments"].(map[string]any)
	}
	if args == nil {
		return tool{}, nil, false
	}
	for _, t := range s.tools {
		if excludedClientTool(t) {
			continue
		}
		if (!indirect && t.mcpName() == name) || (indirect && t.key() == name) {
			if !s.policy.permits(t) {
				return tool{}, nil, false
			}
			if isNativeClientTool(t.Type) {
				if !nativeArgumentsValid(t, args) {
					return tool{}, nil, false
				}
			} else if t.Type == "custom" {
				if _, ok := args["input"].(string); !ok {
					return tool{}, nil, false
				}
			} else if !validArguments(normalizeSchema(t.Parameters), args, 0) {
				return tool{}, nil, false
			}
			return t, args, true
		}
	}
	return tool{}, nil, false
}

// Only retries of a call for which we already emitted a still-running notice
// reuse an existing result. Ordinary parallel identical calls keep distinct IDs.
// The caller holds s.mu, so the allowlist check and reservation are atomic.
func parkToolCall(s *session, name string, args map[string]any) (*pendingTool, bool) {
	t, args, ok := allowedToolCall(s, name, args)
	if !ok {
		return nil, false
	}
	encoded, _ := json.Marshal(args)
	if len(encoded) > MaxText {
		return nil, false
	}
	for _, existing := range s.pending {
		if !existing.Relayed || existing.Tool.key() != t.key() || digest(existing.Arguments) != digest(args) {
			continue
		}
		select {
		case <-existing.Ready:
		default:
			return existing, false
		}
	}
	if s.policy.Serial {
		if len(s.officialActive) > 0 {
			return nil, false
		}
		for _, existing := range s.pending {
			select {
			case <-existing.Ready:
			default:
				return nil, false
			}
		}
	}
	if len(s.pending) >= 128 {
		return nil, false
	}
	call := &pendingTool{ID: newID("call_"), Tool: t, Arguments: args, Ready: make(chan struct{})}
	s.pending[call.ID] = call
	s.policy.Used = true
	return call, true
}
