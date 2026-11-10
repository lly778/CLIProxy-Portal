package agy

import (
	"encoding/json"
	"sort"
	"strings"
)

// Keep policy separate from declarations: a per-request restriction must not
// permanently revoke tools, including tools discovered in an earlier step.
type toolPolicy struct {
	Mode   string
	Names  map[string]bool
	Serial bool
	Used   bool
}

func requestToolPolicy(r request, tools []tool) (toolPolicy, error) {
	p := toolPolicy{Mode: "auto", Serial: r.Parallel != nil && !*r.Parallel}
	if len(r.ToolChoice) == 0 || string(r.ToolChoice) == "null" {
		return p, nil
	}
	var mode string
	if json.Unmarshal(r.ToolChoice, &mode) == nil {
		switch mode {
		case "auto", "none", "required":
			p.Mode = mode
			return p, nil
		}
		return p, fail(400, "invalid_tool_choice", "工具选择必须为 auto、none、required 或指定客户端工具")
	}
	var choice struct {
		Type, Name, Namespace, Mode string
		Tools                       []map[string]any
	}
	if json.Unmarshal(r.ToolChoice, &choice) != nil {
		return p, fail(400, "invalid_tool_choice", "工具选择格式无效")
	}
	if choice.Type == "web_search" || choice.Type == "web_search_preview" || choice.Type == "image_generation" {
		p.Mode = "required"
		p.Names = map[string]bool{"google.generate_image": true}
		if choice.Type != "image_generation" {
			p.Names = map[string]bool{"google.search_web": true, "google.read_url_content": true}
		}
		return p, nil
	}
	p.Names = map[string]bool{}
	add := func(name, namespace string) bool {
		for _, t := range tools {
			if name == t.key() || (name == t.Name && (namespace == t.Namespace || namespace == "")) {
				if namespace == "" && name == t.Name && t.Namespace != "" {
					// Unqualified names must be unambiguous.
					for _, other := range tools {
						if other.Name == name && other.key() != t.key() {
							return false
						}
					}
				}
				p.Names[t.key()] = true
				return true
			}
		}
		return false
	}
	if choice.Type == "allowed_tools" {
		if choice.Mode != "auto" && choice.Mode != "required" {
			return p, fail(400, "invalid_tool_choice", "工具范围的 mode 必须为 auto 或 required")
		}
		p.Mode = choice.Mode
		for _, t := range choice.Tools {
			kind, _ := t["type"].(string)
			if kind == "web_search" || kind == "web_search_preview" {
				p.Names["google.search_web"], p.Names["google.read_url_content"] = true, true
				continue
			}
			if kind == "image_generation" {
				p.Names["google.generate_image"] = true
				continue
			}
			name, _ := t["name"].(string)
			if name == "" {
				kind, _ := t["type"].(string)
				if isNativeClientTool(kind) {
					name = "client_" + kind
				}
				if kind == "tool_search" {
					name = "client_tool_search"
				}
			}
			ns, _ := t["namespace"].(string)
			if !add(name, ns) {
				return p, fail(400, "invalid_tool_choice", "工具范围包含未声明或有歧义的客户端工具")
			}
		}
	} else {
		if choice.Type != "function" && choice.Type != "custom" && !isNativeClientTool(choice.Type) && choice.Type != "tool_search" {
			return p, fail(400, "invalid_tool_choice", "不支持该工具选择类型")
		}
		name := choice.Name
		if name == "" && choice.Type != "function" && choice.Type != "custom" {
			name = "client_" + choice.Type
			if name == "tool_search" {
				name = "client_tool_search"
			}
		}
		if !add(name, choice.Namespace) {
			return p, fail(400, "invalid_tool_choice", "指定的客户端工具未声明或名称有歧义")
		}
		p.Mode = "required"
	}
	if len(p.Names) == 0 {
		return p, fail(400, "invalid_tool_choice", "工具范围不能为空")
	}
	return p, nil
}

func (p toolPolicy) permits(t tool) bool {
	return p.Mode != "none" && (p.Names == nil || p.Names[t.key()])
}
func (p toolPolicy) text() string {
	if p.Mode == "" || p.Mode == "auto" && p.Names == nil && !p.Serial {
		return ""
	}
	parts := []string{"[Tool policy: applies to this request and official Google tools]"}
	if p.Mode == "none" {
		parts = append(parts, "Do not call any tool. Answer without tools.")
	} else {
		if p.Names != nil {
			names := []string{}
			for name := range p.Names {
				names = append(names, name)
			}
			sort.Strings(names)
			parts = append(parts, "Only these tools are permitted (google.NAME means the official built-in NAME): "+strings.Join(names, ", "))
		}
		if p.Mode == "required" {
			parts = append(parts, "Use a permitted tool before finishing this request.")
		}
	}
	if p.Serial {
		parts = append(parts, "Call at most one client tool at a time. Wait for its result before issuing another call.")
	}
	return strings.Join(parts, "\n")
}

func isClientSearch(t tool) bool {
	return t.Type == "tool_search" || t.Type == "function" && t.Name == "ToolSearch"
}
