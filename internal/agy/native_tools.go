package agy

import (
	"encoding/json"
	"fmt"
	"math"
)

// These declarations require the requesting client's executor. This package
// only converts schemas and wire items; it never runs commands, patches or UI.
func isNativeClientTool(kind string) bool {
	switch kind {
	case "local_shell", "shell", "apply_patch", "computer", "computer_use_preview":
		return true
	}
	return false
}

func schemaObject(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func schemaArray(items any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}
func schemaEnum(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
func schemaType(kind string) map[string]any { return map[string]any{"type": kind} }

func nativeClientTool(t tool) (tool, error) {
	if t.Namespace != "" {
		return t, fail(400, "invalid_native_tool", "原生客户端工具须在顶层声明")
	}
	t.Name = "client_" + t.Type
	t.Description = "Ask the requesting client to perform this operation in its own environment and return the actual result. Never perform it on the AGY server."
	text, number := schemaType("string"), schemaType("integer")
	switch t.Type {
	case "shell":
		var environment struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(t.Environment, &environment) != nil || environment.Type != "local" {
			return t, fail(400, "unsupported_shell_environment", "AGY 仅接入客户端提供的本机 Shell，请声明 environment.type=local")
		}
		action := schemaObject(map[string]any{"commands": schemaArray(text), "timeout_ms": number, "max_output_length": number}, "commands")
		t.Parameters = schemaObject(map[string]any{"action": action}, "action")
	case "local_shell":
		action := schemaObject(map[string]any{"type": schemaEnum("exec"), "command": schemaArray(text), "env": map[string]any{"type": "object", "additionalProperties": text}, "timeout_ms": number, "working_directory": text, "user": text}, "type", "command", "env")
		t.Parameters = schemaObject(map[string]any{"action": action}, "action")
	case "apply_patch":
		operation := schemaObject(map[string]any{"type": schemaEnum("create_file", "update_file", "delete_file"), "path": text, "diff": text}, "type", "path")
		t.Parameters = schemaObject(map[string]any{"operation": operation}, "operation")
	case "computer", "computer_use_preview":
		if t.DisplayWidth < 0 || t.DisplayHeight < 0 || (t.Type == "computer_use_preview" && (t.DisplayWidth == 0 || t.DisplayHeight == 0)) {
			return t, fail(400, "invalid_computer_display", "电脑操作的显示尺寸无效")
		}
		if t.DisplayWidth > 0 && t.DisplayHeight > 0 {
			t.Description += fmt.Sprintf(" The client display is %d pixels wide and %d pixels high.", t.DisplayWidth, t.DisplayHeight)
		}
		t.Description += " Actions: click(x,y,button), double_click/move(x,y), drag(path), scroll(x,y,scroll_x,scroll_y), keypress(keys), type(text), wait, screenshot."
		point := schemaObject(map[string]any{"x": number, "y": number}, "x", "y")
		actions := []any{
			schemaObject(map[string]any{"type": schemaEnum("click"), "x": number, "y": number, "button": schemaEnum("left", "right", "wheel", "back", "forward")}, "type", "x", "y", "button"),
			schemaObject(map[string]any{"type": schemaEnum("double_click", "move"), "x": number, "y": number}, "type", "x", "y"),
			schemaObject(map[string]any{"type": schemaEnum("drag"), "path": schemaArray(point)}, "type", "path"),
			schemaObject(map[string]any{"type": schemaEnum("scroll"), "x": number, "y": number, "scroll_x": number, "scroll_y": number}, "type", "x", "y", "scroll_x", "scroll_y"),
			schemaObject(map[string]any{"type": schemaEnum("keypress"), "keys": schemaArray(text)}, "type", "keys"),
			schemaObject(map[string]any{"type": schemaEnum("type"), "text": text}, "type", "text"),
			schemaObject(map[string]any{"type": schemaEnum("wait", "screenshot")}, "type"),
		}
		action := map[string]any{"anyOf": actions}
		if t.Type == "computer" {
			t.Parameters = schemaObject(map[string]any{"actions": schemaArray(action)}, "actions")
		} else {
			t.Parameters = schemaObject(map[string]any{"action": action}, "action")
		}
	}
	return t, nil
}

// A flat MCP presentation avoids AGY lazily hiding a large computer anyOf
// declaration. The exact per-action union above remains the validation schema.
func nativeComputerMCPSchema(t tool) map[string]any {
	number := schemaType("integer")
	point := schemaObject(map[string]any{"x": number, "y": number}, "x", "y")
	action := schemaObject(map[string]any{
		"type": schemaEnum("click", "double_click", "drag", "move", "scroll", "keypress", "type", "wait", "screenshot"),
		"x":    number, "y": number, "button": schemaEnum("left", "right", "wheel", "back", "forward"),
		"path": schemaArray(point), "scroll_x": number, "scroll_y": number,
		"keys": schemaArray(schemaType("string")), "text": schemaType("string"),
	}, "type")
	if t.Type == "computer" {
		return schemaObject(map[string]any{"actions": schemaArray(action)}, "actions")
	}
	return schemaObject(map[string]any{"action": action}, "action")
}

func nativeCallType(kind string) string {
	switch kind {
	case "shell":
		return "shell_call"
	case "local_shell":
		return "local_shell_call"
	case "apply_patch":
		return "apply_patch_call"
	case "computer", "computer_use_preview":
		return "computer_call"
	}
	return ""
}

func nativeResultType(kind string) string {
	if call := nativeCallType(kind); call != "" {
		return call + "_output"
	}
	return ""
}

func isNativeCallItem(kind string) bool {
	return kind == "shell_call" || kind == "local_shell_call" || kind == "apply_patch_call" || kind == "computer_call"
}
func isToolResultItem(kind string) bool {
	return kind == "function_call_output" || kind == "custom_tool_call_output" || kind == "tool_search_output" || kind == "shell_call_output" || kind == "local_shell_call_output" || kind == "apply_patch_call_output" || kind == "computer_call_output"
}

func nativeArgumentsValid(t tool, args map[string]any) bool {
	if !validArguments(normalizeSchema(t.Parameters), args, 0) {
		return false
	}
	positiveOptional := func(object map[string]any, key string) bool {
		value, exists := object[key]
		if !exists {
			return true
		}
		n, ok := value.(float64)
		return ok && n > 0 && n <= math.MaxInt32 && n == math.Trunc(n)
	}
	stringsValid := func(value any) bool {
		list, ok := value.([]any)
		if !ok || len(list) == 0 || len(list) > 256 {
			return false
		}
		for _, v := range list {
			if text, ok := v.(string); !ok || text == "" {
				return false
			}
		}
		return true
	}
	switch t.Type {
	case "shell", "local_shell":
		action, _ := args["action"].(map[string]any)
		key := "commands"
		if t.Type == "local_shell" {
			key = "command"
		}
		if !stringsValid(action[key]) || !positiveOptional(action, "timeout_ms") || !positiveOptional(action, "max_output_length") {
			return false
		}
		if t.Type == "local_shell" {
			env, ok := action["env"].(map[string]any)
			if !ok {
				return false
			}
			for _, value := range env {
				if _, ok := value.(string); !ok {
					return false
				}
			}
		}
	case "apply_patch":
		op, _ := args["operation"].(map[string]any)
		if path, _ := op["path"].(string); path == "" {
			return false
		}
		if op["type"] == "delete_file" {
			if _, exists := op["diff"]; exists {
				return false
			}
		} else {
			if _, ok := op["diff"].(string); !ok {
				return false
			}
		}
	case "computer", "computer_use_preview":
		actions := []any{args["action"]}
		if t.Type == "computer" {
			actions, _ = args["actions"].([]any)
		}
		if len(actions) == 0 || len(actions) > 64 {
			return false
		}
		pointValid := func(point map[string]any) bool {
			x, xOK := point["x"].(float64)
			y, yOK := point["y"].(float64)
			return xOK && yOK && x >= 0 && y >= 0 && x <= math.MaxInt32 && y <= math.MaxInt32 && (t.DisplayWidth == 0 || x < float64(t.DisplayWidth)) && (t.DisplayHeight == 0 || y < float64(t.DisplayHeight))
		}
		for _, raw := range actions {
			action, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			switch action["type"] {
			case "click", "double_click", "move", "scroll":
				if !pointValid(action) {
					return false
				}
			case "drag":
				path, _ := action["path"].([]any)
				if len(path) < 2 || len(path) > 1024 {
					return false
				}
				for _, v := range path {
					point, ok := v.(map[string]any)
					if !ok || !pointValid(point) {
						return false
					}
				}
			case "keypress":
				if !stringsValid(action["keys"]) {
					return false
				}
			}
		}
	}
	return true
}

func nativeOutputItem(c *pendingTool) map[string]any {
	kind := nativeCallType(c.Tool.Type)
	item := map[string]any{"id": c.ID, "type": kind, "call_id": c.ID, "status": "completed"}
	switch c.Tool.Type {
	case "shell":
		action, _ := c.Arguments["action"].(map[string]any)
		item["action"] = map[string]any{"commands": action["commands"], "timeout_ms": action["timeout_ms"], "max_output_length": action["max_output_length"]}
	case "local_shell":
		item["action"] = c.Arguments["action"]
	case "apply_patch":
		item["operation"] = c.Arguments["operation"]
	case "computer":
		item["actions"] = c.Arguments["actions"]
	case "computer_use_preview":
		item["action"] = c.Arguments["action"]
		item["pending_safety_checks"] = []any{}
	}
	return item
}

func resultCallID(item map[string]any) string {
	id, _ := item["call_id"].(string)
	if id == "" && item["type"] == "local_shell_call_output" {
		id, _ = item["id"].(string)
	}
	return id
}

func nativeResultValue(c *pendingTool, item map[string]any) (any, error) {
	invalid := func() (any, error) {
		return nil, fail(400, "invalid_native_tool_result", "原生客户端工具结果类型或内容无效")
	}
	if item["type"] != nativeResultType(c.Tool.Type) {
		return invalid()
	}
	switch c.Tool.Type {
	case "shell":
		output, ok := item["output"].([]any)
		if !ok || len(output) == 0 || len(output) > 256 {
			return invalid()
		}
		action, _ := c.Arguments["action"].(map[string]any)
		if commands, ok := action["commands"].([]any); !ok || len(output) != len(commands) {
			return invalid()
		}
		for _, raw := range output {
			entry, ok := raw.(map[string]any)
			if !ok {
				return invalid()
			}
			if _, ok := entry["stdout"].(string); !ok {
				return invalid()
			}
			if _, ok := entry["stderr"].(string); !ok {
				return invalid()
			}
			outcome, _ := entry["outcome"].(map[string]any)
			if outcome["type"] != "timeout" {
				n, ok := outcome["exit_code"].(float64)
				if outcome["type"] != "exit" || !ok || math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || math.Abs(n) > math.MaxInt32 {
					return invalid()
				}
			}
		}
		if expected, ok := action["max_output_length"]; ok && item["max_output_length"] != expected {
			return invalid()
		}
		return map[string]any{"output": output, "max_output_length": item["max_output_length"]}, nil
	case "local_shell":
		if id, exists := item["id"]; exists && id != c.ID {
			return invalid()
		}
		text, ok := item["output"].(string)
		if !ok {
			return invalid()
		}
		if status := item["status"]; status != nil && status != "completed" && status != "incomplete" {
			return invalid()
		}
		return map[string]any{"output": text, "status": item["status"]}, nil
	case "apply_patch":
		text, ok := item["output"].(string)
		if !ok || (item["status"] != "completed" && item["status"] != "failed") {
			return invalid()
		}
		return map[string]any{"output": text, "status": item["status"]}, nil
	case "computer", "computer_use_preview":
		if checks, exists := item["acknowledged_safety_checks"]; exists {
			list, ok := checks.([]any)
			if !ok || len(list) > 0 {
				return invalid()
			}
		}
		screenshot, ok := item["output"].(map[string]any)
		if !ok || screenshot["type"] != "computer_screenshot" {
			return invalid()
		}
		if _, err := toolResultBlocks(screenshot); err != nil {
			return nil, err
		}
		return screenshot, nil
	}
	return invalid()
}
