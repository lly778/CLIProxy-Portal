package agy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAGYLegacySearchAndImageExecutorsCannotReenter(t *testing.T) {
	for _, name := range []string{"WebSearch", "web_fetch", "generate_image", "mcp__codex_apps__search_service_web_run", "mcp__image_gen__imagegen", "mcp__exa__search", "web.run"} {
		t.Run(name, func(t *testing.T) {
			decl := tool{Type: "function", Name: name, Parameters: map[string]any{"type": "object"}}
			b, _ := json.Marshal(map[string]any{"model": "agy/test", "input": "test", "tools": []tool{decl}})
			r, err := parseRequest(b)
			if err != nil || len(r.Tools) != 0 {
				t.Fatal("legacy executor survived initial declaration")
			}
			loaded, err := parseLoadedTools([]any{map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}})
			if err != nil || len(loaded) != 0 {
				t.Fatal("legacy executor survived dynamic discovery")
			}
			s := &session{tools: []tool{decl}, loadedTools: []tool{decl}, pending: map[string]*pendingTool{}}
			if _, _, ok := allowedToolCall(s, decl.mcpName(), map[string]any{}); ok {
				t.Fatal("frozen legacy descriptor remained callable")
			}
			base, loaded := requestedTools(s, request{})
			if len(base) != 0 || len(loaded) != 0 {
				t.Fatal("omitted tools restored cached legacy executors")
			}
			refs := []any{map[string]any{"type": "tool_reference", "tool_name": name}}
			if got, err := referencedTools(refs, []tool{decl}); err != nil || len(got) != 0 {
				t.Fatal("legacy tool reference reactivated executor")
			}
			clean, _ := json.Marshal(filterDiscoveryResult(refs, 0))
			if strings.Contains(string(clean), name) {
				t.Fatal("legacy discovery reference still advertised")
			}
		})
	}
	for _, name := range []string{"view_image", "search_files", "document_search", "mcp__files__read_resource", "mcp__remote__lookup", "exec_command"} {
		if excludedClientTool(tool{Name: name}) {
			t.Fatalf("unrelated client tool removed: %s", name)
		}
	}
}
