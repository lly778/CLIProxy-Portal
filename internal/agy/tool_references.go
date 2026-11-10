package agy

import (
	"encoding/json"
	"sort"
)

func referencedTools(output any, declared []tool) ([]tool, error) {
	names := map[string]bool{}
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 32 {
			return
		}
		switch node := value.(type) {
		case string:
			var parsed any
			if json.Unmarshal([]byte(node), &parsed) == nil {
				if _, same := parsed.(string); !same {
					walk(parsed, depth+1)
				}
			}
		case []any:
			for _, v := range node {
				walk(v, depth+1)
			}
		case map[string]any:
			if node["type"] == "tool_reference" {
				if name, ok := node["tool_name"].(string); ok {
					names[name] = true
				}
			}
			if value, ok := node["content"]; ok {
				walk(value, depth+1)
			}
		}
	}
	walk(output, 0)
	tools := []tool{}
	ordered := []string{}
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		if excludedClientTool(tool{Name: name}) {
			continue
		}
		var match *tool
		for _, t := range declared {
			if name == t.key() || name == t.Name {
				if t.Type != "function" && t.Type != "custom" {
					return nil, fail(400, "invalid_tool_reference", "工具引用只能指向客户端函数或自定义工具")
				}
				if match != nil && match.key() != t.key() {
					return nil, fail(400, "invalid_tool_reference", "工具引用名称有歧义")
				}
				v := t
				match = &v
			}
		}
		if match == nil {
			return nil, fail(400, "invalid_tool_reference", "工具引用没有匹配的客户端声明")
		}
		match.Deferred = false
		tools = append(tools, *match)
	}
	return tools, nil
}
