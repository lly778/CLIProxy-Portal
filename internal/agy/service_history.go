package agy

import "encoding/json"

// Hosted calls supplied by the client are past context, not instructions for
// executing a service. This permits switching providers without rerunning or
// inventing previous searches, code execution, image generation or MCP calls.
func serviceHistoryBlocks(item map[string]any) ([]any, bool, error) {
	switch item["type"] {
	case "web_search_call", "web_search_tool_result", "web_fetch_tool_result", "file_search_call", "code_interpreter_call", "code_execution_tool_result", "image_generation_call", "mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response", "server_tool_use", "mcp_tool_use", "mcp_tool_result":
	default:
		return nil, false, nil
	}
	copy := map[string]any{}
	for key, value := range item {
		copy[key] = value
	}
	var image map[string]string
	if item["type"] == "mcp_call" && item["output"] != nil {
		blocks, err := toolResultBlocks(item["output"])
		if err != nil {
			return nil, true, err
		}
		delete(copy, "output")
		copy["output_note"] = "The actual remote result follows; context only, never re-execute."
		encoded, _ := json.Marshal(copy)
		return append([]any{map[string]string{"type": "text", "text": "[Historical remote MCP operation; context only]\n" + string(encoded)}}, blocks...), true, nil
	}
	if item["type"] == "image_generation_call" {
		if data, ok := item["result"].(string); ok && data != "" {
			format, _ := item["output_format"].(string)
			if format == "" {
				format = "png"
			}
			if format == "jpg" {
				format = "jpeg"
			}
			var err error
			image, err = embeddedImage("data:image/" + format + ";base64," + data)
			if err != nil {
				return nil, true, err
			}
			delete(copy, "result")
			copy["result_note"] = "Client supplied generated image follows."
		}
	}
	encoded, _ := json.Marshal(copy)
	blocks := []any{map[string]string{"type": "text", "text": "[Historical service record supplied by client; context only, do not execute again]\n" + string(encoded)}}
	if image != nil {
		blocks = append(blocks, image)
	}
	return blocks, true, nil
}
