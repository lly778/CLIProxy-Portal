package agy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Preserve client search results/artifacts without fetching URLs or opening
// paths on the gateway host. Pixels must come from client-supplied image data.
func clientContentBlock(node map[string]any, depth int) ([]any, bool, error) {
	text := func(v string) ([]any, bool, error) {
		return []any{map[string]string{"type": "text", "text": v}}, true, nil
	}
	image := func(raw string) ([]any, bool, error) {
		v, err := embeddedImage(raw)
		if err != nil {
			return nil, true, err
		}
		return []any{v}, true, nil
	}
	switch node["type"] {
	case "image":
		if source, ok := node["source"].(map[string]any); ok {
			if source["type"] == "base64" {
				mime, _ := source["media_type"].(string)
				data, _ := source["data"].(string)
				return image("data:" + mime + ";base64," + data)
			}
			if source["type"] == "url" {
				uri, _ := source["url"].(string)
				return text("[Client image artifact URL; image pixels not loaded]\n" + uri)
			}
		}
	case "image_url":
		value := node["image_url"]
		raw, _ := value.(string)
		if object, ok := value.(map[string]any); ok {
			raw, _ = object["url"].(string)
		}
		if strings.HasPrefix(raw, "data:") {
			return image(raw)
		}
		if raw != "" {
			return text("[Client image artifact URL; image pixels not loaded]\n" + raw)
		}
		return nil, true, fail(400, "invalid_image", "客户端图片结果缺少图片数据或地址")
	case "resource":
		resource, ok := node["resource"].(map[string]any)
		if !ok {
			return nil, true, fail(400, "invalid_tool_result", "MCP 资源结果格式无效")
		}
		uri, _ := resource["uri"].(string)
		if body, ok := resource["text"].(string); ok {
			return text("[Client resource " + uri + "]\n" + body)
		}
		if blob, ok := resource["blob"].(string); ok {
			mime, _ := resource["mimeType"].(string)
			if strings.HasPrefix(mime, "image/") {
				return image("data:" + mime + ";base64," + blob)
			}
		}
		// Keep non-image binary artifacts as references, not giant base64 text.
		if uri != "" {
			copy := map[string]any{}
			for k, v := range resource {
				if k != "blob" {
					copy[k] = v
				}
			}
			encoded, _ := json.Marshal(copy)
			return text("[Client binary resource reference; content not loaded]\n" + string(encoded))
		}
		return nil, true, fail(400, "invalid_tool_result", "二进制资源缺少可引用的地址")
	case "resource_link":
		encoded, _ := json.Marshal(node)
		return text("[Client resource link; not fetched by the gateway]\n" + string(encoded))
	case "document":
		source, ok := node["source"].(map[string]any)
		if !ok {
			return nil, true, fail(400, "unsupported_document", "请由客户端提取文档文字后回传")
		}
		body, _ := source["data"].(string)
		if source["type"] == "base64" {
			mime, _ := source["media_type"].(string)
			if !strings.HasPrefix(mime, "text/") {
				return nil, true, fail(400, "unsupported_document", "二进制文档需由客户端提取文字后回传")
			}
			decoded, err := base64.StdEncoding.DecodeString(body)
			if err != nil || len(decoded) > MaxBody || !utf8.Valid(decoded) {
				return nil, true, fail(400, "invalid_document", "文档文字编码无效")
			}
			body = string(decoded)
		} else if source["type"] != "text" {
			return nil, true, fail(400, "unsupported_document", "请由客户端提取文档文字后回传")
		}
		title, _ := node["title"].(string)
		return text("[Client document " + title + "]\n" + body)
	case "search_result":
		blocks, err := toolResultBlocksDepth(node["content"], depth+1)
		if err != nil {
			return nil, true, err
		}
		source, _ := node["source"].(string)
		title, _ := node["title"].(string)
		return append([]any{map[string]string{"type": "text", "text": "[Client search result]\nTitle: " + title + "\nSource: " + source}}, blocks...), true, nil
	}
	return nil, false, nil
}
