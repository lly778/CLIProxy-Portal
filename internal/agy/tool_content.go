package agy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

func embeddedImage(raw string) (map[string]string, error) {
	if !strings.HasPrefix(raw, "data:image/") {
		return nil, fail(400, "image_url_unsupported", "AGY 图片需使用内嵌图片数据，不支持远程地址或文件引用")
	}
	parts := strings.SplitN(raw, ",", 2)
	if len(parts) != 2 || !strings.HasSuffix(parts[0], ";base64") {
		return nil, fail(400, "invalid_image", "图片编码无效")
	}
	mime := strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64")
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" && mime != "image/gif" {
		return nil, fail(400, "invalid_image", "不支持该图片格式")
	}
	decoded, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(decoded) == 0 || len(decoded) > MaxBody {
		return nil, fail(400, "invalid_image", "图片数据无效或过大")
	}
	return map[string]string{"type": "image", "mimeType": mime, "data": parts[1]}, nil
}

// Convert Responses content and MCP-style image content into real ACP/MCP
// blocks. Client tool results never cause URL fetches or local path reads.
func toolResultBlocks(value any) ([]any, error) {
	return toolResultBlocksDepth(value, 0)
}

func toolResultBlocksDepth(value any, depth int) ([]any, error) {
	if depth > 16 {
		return nil, fail(400, "invalid_tool_result", "工具结果嵌套过深")
	}
	if node, ok := value.(map[string]any); ok {
		_, hasError := node["isError"]
		_, hasLegacyError := node["is_error"]
		_, hasStructured := node["structuredContent"]
		if content, ok := node["content"].([]any); ok && (hasError || hasLegacyError || hasStructured || node["type"] == "tool_result" || len(node) == 1) {
			blocks, err := toolResultBlocksDepth(content, depth+1)
			if err != nil {
				return nil, err
			}
			if structured, ok := node["structuredContent"]; ok {
				b, _ := json.Marshal(structured)
				blocks = append(blocks, map[string]string{"type": "text", "text": "[Client structured result]\n" + string(b)})
			}
			return blocks, nil
		}
	}
	if text, ok := value.(string); ok {
		var envelope map[string]any
		if len(text) <= MaxBody && json.Unmarshal([]byte(text), &envelope) == nil {
			if _, ok := envelope["content"].([]any); ok && (envelope["isError"] != nil || envelope["is_error"] != nil || envelope["structuredContent"] != nil) {
				return toolResultBlocksDepth(envelope, depth+1)
			}
		}
		return []any{map[string]string{"type": "text", "text": text}}, nil
	}
	parts, ok := value.([]any)
	if !ok {
		parts = []any{value}
	}
	blocks := make([]any, 0, len(parts))
	images := 0
	for _, part := range parts {
		if node, ok := part.(map[string]any); ok {
			if extra, handled, err := clientContentBlock(node, depth); handled {
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, extra...)
				for _, raw := range extra {
					if image, ok := raw.(map[string]string); ok && image["type"] == "image" {
						images++
					}
				}
				continue
			}
			switch node["type"] {
			case "input_text", "output_text", "text":
				text, ok := node["text"].(string)
				if !ok {
					return nil, fail(400, "invalid_tool_result", "工具结果文字格式无效")
				}
				blocks = append(blocks, map[string]string{"type": "text", "text": text})
				continue
			case "input_image", "image", "computer_screenshot":
				raw, _ := node["image_url"].(string)
				if node["type"] == "image" && raw == "" {
					mime, _ := node["mimeType"].(string)
					if mime == "" {
						mime, _ = node["mime_type"].(string)
					}
					data, _ := node["data"].(string)
					raw = "data:" + mime + ";base64," + data
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
				continue
			}
		}
		encoded, err := json.Marshal(part)
		if err != nil {
			return nil, fail(400, "invalid_tool_result", "工具结果格式无效")
		}
		blocks = append(blocks, map[string]string{"type": "text", "text": string(encoded)})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]string{"type": "text", "text": "(no output)"})
	}
	if images > 64 {
		return nil, fail(413, "too_many_images", "图片数量超过 64 张")
	}
	return blocks, nil
}
