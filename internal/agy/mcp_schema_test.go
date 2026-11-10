package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAGYMCPProjectsEnumsWithoutChangingClientValidation(t *testing.T) {
	var original map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"allowAsync":{"type":"boolean","enum":[false],"description":"Run synchronously."},"count":{"type":"integer","enum":[1,2]},"mode":{"type":"string","enum":["read","write"]},"empty":{"type":"string","enum":[""]},"nested":{"type":"array","items":{"oneOf":[{"type":"boolean","enum":[true]}]}},"literal":{"const":{"enum":[false]}}},"$defs":{"flag":{"type":"boolean","enum":[false]}},"required":["allowAsync"]}`), &original); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(original)
	s := &session{secret: "fixture-secret", tools: []tool{{Type: "function", Name: "probe", Parameters: original}}}
	m := &Manager{sessions: map[string]*session{"fixture": s}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/mcp/fixture", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.Header.Set("Authorization", "Bearer fixture-secret")
	m.mcp(w, r)
	var reply struct {
		Result struct {
			Tools []struct {
				Schema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || len(reply.Result.Tools) != 4 {
		t.Fatalf("MCP tools/list failed: %d %s %v", w.Code, w.Body.String(), err)
	}
	projected := reply.Result.Tools[0].Schema
	properties := projected["properties"].(map[string]any)
	for _, name := range []string{"allowAsync", "count", "empty"} {
		node := properties[name].(map[string]any)
		if _, exists := node["enum"]; exists || !strings.Contains(node["description"].(string), "Allowed values:") {
			t.Fatalf("incompatible enum not projected: %s %+v", name, node)
		}
	}
	if !strings.Contains(properties["allowAsync"].(map[string]any)["description"].(string), "[false]") {
		t.Fatal("boolean constraint not explained to the model")
	}
	if _, exists := properties["mode"].(map[string]any)["enum"]; !exists {
		t.Fatal("valid string enum lost")
	}
	items := properties["nested"].(map[string]any)["items"].(map[string]any)
	if _, exists := items["enum"]; exists || !strings.Contains(items["description"].(string), "Allowed values:") {
		t.Fatal("nested schema not projected")
	}
	if _, exists := projected["$defs"]; exists {
		t.Fatal("unused local definitions leaked into the model-facing schema")
	}
	if !strings.Contains(properties["literal"].(map[string]any)["description"].(string), `"enum":[false]`) {
		t.Fatal("literal const constraint was lost")
	}
	after, _ := json.Marshal(original)
	if string(before) != string(after) || !validArguments(original, map[string]any{"allowAsync": false}, 0) || validArguments(original, map[string]any{"allowAsync": true}, 0) {
		t.Fatal("original client validation changed")
	}
}
