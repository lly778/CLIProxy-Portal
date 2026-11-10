package agy

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// Projection is only the model-facing description. Dispatch always checks the
// original client schema. Never resolve a network/file reference.
func schemaPointer(root map[string]any, ref string) (map[string]any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var value any = root
	for _, part := range strings.Split(ref[2:], "/") {
		node, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		value = node[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
	}
	node, ok := value.(map[string]any)
	return node, ok
}

func schemaHint(node map[string]any, hint string) {
	if hint == "" {
		return
	}
	old, _ := node["description"].(string)
	if strings.Contains(old, hint) {
		return
	}
	if old != "" {
		old += "\n"
	}
	node["description"] = old + hint
}

func schemaRequired(node map[string]any) []string {
	out := []string{}
	if list, ok := node["required"].([]any); ok {
		for _, v := range list {
			if name, ok := v.(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func mergeProjected(nodes []map[string]any, all bool) map[string]any {
	out := map[string]any{}
	props := map[string]any{}
	counts := map[string]int{}
	types := map[string]bool{}
	for _, node := range nodes {
		if typ, ok := node["type"].(string); ok {
			types[typ] = true
		}
		for key, value := range node {
			if key != "properties" && key != "required" && key != "description" {
				if _, exists := out[key]; !exists {
					out[key] = value
				}
			}
		}
		if text, ok := node["description"].(string); ok {
			schemaHint(out, text)
		}
		if ps, ok := node["properties"].(map[string]any); ok {
			for key, value := range ps {
				if prev, exists := props[key].(map[string]any); exists {
					if current, ok := value.(map[string]any); ok {
						props[key] = mergeProjected([]map[string]any{prev, current}, all)
					}
				} else {
					props[key] = value
				}
			}
		}
		seen := map[string]bool{}
		for _, key := range schemaRequired(node) {
			if !seen[key] {
				counts[key]++
				seen[key] = true
			}
		}
		// Bounds on intersections must be tightened, not silently overwritten.
		if all {
			for _, key := range []string{"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties"} {
				v, ok := node[key].(float64)
				if !ok {
					continue
				}
				prev, exists := out[key].(float64)
				if !exists || strings.HasPrefix(key, "min") && v > prev || strings.HasPrefix(key, "max") && v < prev {
					out[key] = v
				}
			}
		}
	}
	if len(types) > 1 {
		schemaHint(out, "Follow the original union/intersection constraints; alternative types are described below.")
	}
	if len(props) > 0 {
		out["properties"] = props
		out["type"] = "object"
	}
	required := []string{}
	for key, n := range counts {
		if all || n == len(nodes) {
			required = append(required, key)
		}
	}
	sort.Strings(required)
	if len(required) > 0 {
		values := []any{}
		for _, key := range required {
			values = append(values, key)
		}
		out["required"] = values
	}
	return out
}

func mcpInputSchema(original map[string]any) map[string]any {
	if original == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	remaining := 2048
	var visit func(map[string]any, int, map[string]bool) map[string]any
	visit = func(node map[string]any, depth int, active map[string]bool) map[string]any {
		remaining--
		if depth > 32 || remaining < 0 {
			return map[string]any{"type": "string", "description": "Schema expansion limit reached. Follow the original client tool contract."}
		}
		out := map[string]any{}
		for _, key := range []string{"type", "title", "description", "format", "enum", "default", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "required"} {
			if v, ok := node[key]; ok {
				out[key] = v
			}
		}
		if values, ok := node["type"].([]any); ok {
			delete(out, "type")
			for _, v := range values {
				if v == "null" {
					schemaHint(out, "Accepts null.")
				} else if out["type"] == nil {
					out["type"] = v
				}
			}
		}
		if node["type"] == "null" {
			out["type"] = "string"
			schemaHint(out, "Must equal null.")
		}
		if node["nullable"] == true {
			schemaHint(out, "Accepts null.")
		}
		if props, ok := node["properties"].(map[string]any); ok {
			ps := map[string]any{}
			for name, raw := range props {
				if child, ok := raw.(map[string]any); ok {
					ps[name] = visit(child, depth+1, active)
				}
			}
			out["properties"] = ps
			out["type"] = "object"
		}
		if items, ok := node["items"].(map[string]any); ok {
			out["items"] = visit(items, depth+1, active)
			out["type"] = "array"
		}
		for _, key := range []string{"items", "prefixItems"} {
			if tuple, ok := node[key].([]any); ok {
				branches := []map[string]any{}
				for _, raw := range tuple {
					if child, ok := raw.(map[string]any); ok {
						branches = append(branches, visit(child, depth+1, active))
					}
				}
				out["type"] = "array"
				out["items"] = mergeProjected(branches, false)
				b, _ := json.Marshal(tuple)
				schemaHint(out, "Tuple positions: "+shortSchemaText(string(b), 1800))
			}
		}
		if value, ok := node["const"]; ok {
			if text, ok := value.(string); ok && text != "" {
				out["enum"] = []any{text}
			} else {
				b, _ := json.Marshal(value)
				schemaHint(out, "Must equal: "+shortSchemaText(string(b), 1800))
			}
		}
		if ref, ok := node["$ref"].(string); ok {
			if target, ok := schemaPointer(original, ref); ok && !active[ref] {
				next := map[string]bool{}
				for k, v := range active {
					next[k] = v
				}
				next[ref] = true
				out = mergeProjected([]map[string]any{visit(target, depth+1, next), out}, true)
			} else {
				schemaHint(out, "Recursive or unresolved reference: "+ref+". Follow the original client contract.")
			}
		}
		for _, kind := range []string{"allOf", "anyOf", "oneOf"} {
			if values, ok := node[kind].([]any); ok {
				branches := []map[string]any{}
				for _, raw := range values {
					if child, ok := raw.(map[string]any); ok {
						if child["type"] == "null" && kind != "allOf" {
							schemaHint(out, "Accepts null.")
							continue
						}
						branches = append(branches, visit(child, depth+1, active))
					}
				}
				out = mergeProjected([]map[string]any{mergeProjected(branches, kind == "allOf"), out}, true)
				b, _ := json.Marshal(values)
				schemaHint(out, kind+" constraints: "+shortSchemaText(string(b), 1800))
			}
		}
		for _, key := range []string{"additionalProperties", "patternProperties", "not", "contains", "uniqueItems", "dependentRequired", "if", "then", "else", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "pattern"} {
			if value, ok := node[key]; ok {
				b, _ := json.Marshal(value)
				schemaHint(out, key+": "+shortSchemaText(string(b), 1800))
			}
		}
		if out["type"] == nil {
			var sample any
			if values, ok := node["enum"].([]any); ok {
				for _, v := range values {
					if v != nil {
						sample = v
						break
					}
				}
			} else {
				sample = node["const"]
			}
			switch sample.(type) {
			case string:
				out["type"] = "string"
			case bool:
				out["type"] = "boolean"
			case float64:
				out["type"] = "number"
			case map[string]any:
				out["type"] = "object"
			case []any:
				out["type"] = "array"
			}
		}
		if out["type"] == nil {
			if depth == 0 {
				out["type"] = "object"
			} else {
				out["type"] = "string"
				schemaHint(out, "Accepts any JSON value allowed by the original client contract.")
			}
		}
		return out
	}
	out := projectSchemaEnums(visit(original, 0, map[string]bool{}))
	var clean func(map[string]any)
	clean = func(node map[string]any) {
		ps, _ := node["properties"].(map[string]any)
		if node["type"] != "object" {
			delete(node, "properties")
			delete(node, "required")
		} else {
			req := []any{}
			for _, name := range schemaRequired(node) {
				if _, ok := ps[name]; ok {
					req = append(req, name)
				}
			}
			if len(req) > 0 {
				node["required"] = req
			} else {
				delete(node, "required")
			}
		}
		if node["type"] != "array" {
			delete(node, "items")
		}
		for _, raw := range ps {
			if child, ok := raw.(map[string]any); ok {
				clean(child)
			}
		}
		if child, ok := node["items"].(map[string]any); ok {
			clean(child)
		}
	}
	clean(out)
	return out
}

func shortSchemaText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

// Preserve structural constraints; shorten prose only. The generic dispatcher
// stays small even when the exact schema itself cannot fit the eager budget.
func compactMCPDescriptor(tool map[string]any) map[string]any {
	encoded, _ := json.Marshal(tool)
	if len(encoded) <= 1350 {
		return tool
	}
	out := normalizeSchema(tool)
	for _, limit := range []int{90, 0} {
		var compact func(any)
		compact = func(value any) {
			switch node := value.(type) {
			case map[string]any:
				for key, v := range node {
					if key == "description" || key == "title" || key == "examples" {
						if text, ok := v.(string); ok && limit > 0 {
							node[key] = shortSchemaText(text, limit)
						} else {
							delete(node, key)
						}
					} else {
						compact(v)
					}
				}
			case []any:
				for _, v := range node {
					compact(v)
				}
			}
		}
		compact(out)
		// Keep the qualified client name visible even if prose is removed.
		out["description"] = shortSchemaText(stringField(tool, "description"), 300)
		encoded, _ = json.Marshal(out)
		if len(encoded) <= 1350 {
			break
		}
	}
	encoded, _ = json.Marshal(out)
	if len(encoded) > 1350 {
		return map[string]any{"name": tool["name"], "description": shortSchemaText(stringField(tool, "description"), 180) + " Read " + describeToolName + " for the complete contract before calling this tool.", "inputSchema": map[string]any{"type": "object"}}
	}
	return out
}

func oversizedToolContext(tools []tool) (string, error) {
	entries := []string{}
	for _, t := range tools {
		// Raw declarations include the instructions removed from the eager
		// descriptor. Deliver them once as context, not on every tools/list.
		b, _ := json.Marshal(t)
		if len(b) > 1100 {
			entries = append(entries, string(b))
		}
	}
	if len(entries) == 0 {
		return "", nil
	}
	text := "\n\n[Full client contracts for large tools; declarations only]\n" + strings.Join(entries, "\n") + "\nUse " + callToolName + " with the exact qualified name if a large tool is not directly listed.\n"
	if len(text) > MaxText/2 {
		names := []string{}
		for _, t := range tools {
			names = append(names, t.key())
		}
		encoded, _ := json.Marshal(names)
		text = "\n\n[Large client tool catalog]\n" + string(encoded) + "\nUse " + describeToolName + " to retrieve the complete original contract for a tool before invoking it via " + callToolName + ". No action is executed by describing a tool.\n"
		if len(text) > MaxText/2 {
			return "", fail(413, "tool_definitions_too_large", "工具名称目录超过安全输入上限")
		}
	}
	return text, nil
}
