package agy

import (
	"encoding/json"
	"strings"
)

// Search, public page reads and image generation have one executor: the
// official Google tools. Keep this exclusion at every client-tool entry point,
// including deferred discovery and cached declarations. File search, MCP
// resource access and image viewing are independent client capabilities.
func excludedClientTool(t tool) bool {
	key := strings.ToLower(t.key())
	name := strings.ToLower(t.Name)
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	switch name {
	case "websearch", "web_search", "search_web", "web_search_preview", "web_run", "google_search", "bing_search", "brave_web_search", "tavily_search", "exa_search", "duckduckgo_search",
		"webfetch", "web_fetch", "fetch_url", "read_url", "read_url_content", "fetch_webpage",
		"imagegen", "image_gen", "image_generation", "generate_image", "generate_images", "create_image", "text_to_image", "txt2img", "edit_image":
		return true
	}
	if strings.Contains(key, "search_service") && strings.Contains(name, "web") || strings.HasSuffix(key, "web.run") || strings.Contains(key, "mcp__web__") && name == "run" || strings.HasSuffix(key, "image_gen__imagegen") {
		return true
	}
	// Provider-specific MCP tools often use a generic name such as "search".
	if name == "search" || name == "search_query" || name == "fetch" {
		for _, provider := range []string{"tavily", "brave", "exa", "serper", "duckduckgo", "bing", "google_search"} {
			if strings.Contains(key, "mcp__"+provider+"__") || strings.HasPrefix(key, provider+".") {
				return true
			}
		}
	}
	return false
}

func filterClientTools(tools []tool) []tool {
	if tools == nil {
		return nil
	}
	out := make([]tool, 0, len(tools))
	for _, t := range tools {
		if !excludedClientTool(t) {
			out = append(out, t)
		}
	}
	return out
}

// Tool discovery results may contain references as JSON strings as well as
// structured definitions. Removed tools must not be re-advertised to AGY.
func filterDiscoveryResult(value any, depth int) any {
	if depth > 32 {
		return nil
	}
	switch node := value.(type) {
	case string:
		var parsed any
		if unmarshalExact([]byte(node), &parsed) == nil {
			if _, same := parsed.(string); !same {
				b, _ := json.Marshal(filterDiscoveryResult(parsed, depth+1))
				return string(b)
			}
		}
	case []any:
		out := []any{}
		for _, v := range node {
			if clean := filterDiscoveryResult(v, depth+1); clean != nil {
				out = append(out, clean)
			}
		}
		return out
	case map[string]any:
		if node["type"] == "tool_reference" && excludedClientTool(tool{Name: stringField(node, "tool_name")}) {
			return nil
		}
		if (node["type"] == "function" || node["type"] == "custom") && excludedClientTool(tool{Name: stringField(node, "name"), Namespace: stringField(node, "namespace")}) {
			return nil
		}
		out := map[string]any{}
		for k, v := range node {
			out[k] = filterDiscoveryResult(v, depth+1)
		}
		return out
	}
	return value
}
