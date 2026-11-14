package agy

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAGYSchemaProjectionResolvesReferencesAndRetainsOriginalValidation(t *testing.T) {
	var original map[string]any
	_ = json.Unmarshal([]byte(`{"type":"object","$defs":{"config":{"type":"object","properties":{"enabled":{"type":"boolean","enum":[false]},"count":{"type":"integer"}},"required":["enabled"]}},"properties":{"config":{"$ref":"#/$defs/config"},"nullable":{"anyOf":[{"type":"string"},{"type":"null"}]},"tuple":{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}]}},"required":["config"]}`), &original)
	before := digest(original)
	projected := mcpInputSchema(original)
	encoded, _ := json.Marshal(projected)
	for _, bad := range []string{`"$ref"`, `"$defs"`, `"anyOf":`, `"prefixItems":`} {
		if strings.Contains(string(encoded), bad) {
			t.Fatalf("incompatible schema member retained: %s", bad)
		}
	}
	props := projected["properties"].(map[string]any)
	config := props["config"].(map[string]any)
	if config["type"] != "object" || !strings.Contains(string(encoded), "Accepts null.") || before != digest(original) {
		t.Fatal("reference or nullable semantics lost")
	}
	valid := map[string]any{"config": map[string]any{"enabled": false}}
	if !validArguments(original, valid, 0) {
		t.Fatal("valid reference arguments rejected")
	}
	valid["config"].(map[string]any)["enabled"] = true
	if validArguments(original, valid, 0) {
		t.Fatal("projected enum weakened original reference validation")
	}
}

func TestAGYSchemaProjectionHandlesRequiredOnlyIntersectionsAndBoundedRecursion(t *testing.T) {
	var original map[string]any
	_ = json.Unmarshal([]byte(`{"allOf":[{"type":"object","properties":{"path":{"type":"string"},"count":{"type":"integer","minimum":1,"maximum":9}}},{"required":["path"],"properties":{"count":{"type":"integer","minimum":3,"maximum":7}}}]}`), &original)
	projected := mcpInputSchema(original)
	props := projected["properties"].(map[string]any)
	count := props["count"].(map[string]any)
	if !jsonValueEqual(count["minimum"], float64(3)) || !jsonValueEqual(count["maximum"], float64(7)) || len(schemaRequired(projected)) != 1 || schemaRequired(projected)[0] != "path" {
		t.Fatalf("intersection weakened: %+v", projected)
	}
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"next":{"$ref":"#"},"missing":{"$ref":"https://example.test/schema"}}}`), &original)
	encoded, _ := json.Marshal(mcpInputSchema(original))
	if len(encoded) > 8000 || !strings.Contains(string(encoded), "Recursive or unresolved reference") {
		t.Fatal("recursive projection is unbounded or silent")
	}
}

func TestAGYLargeMCPDescriptorCompactsProseButPreservesStructure(t *testing.T) {
	tool := map[string]any{"name": "probe", "description": strings.Repeat("long description ", 200), "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "description": strings.Repeat("explanation ", 200)}, "mode": map[string]any{"type": "string", "enum": []any{"read", "write"}}}, "required": []any{"path"}}}
	before := digest(tool)
	result := compactMCPDescriptor(tool)
	encoded, _ := json.Marshal(result)
	if len(encoded) > 1350 || before != digest(tool) {
		t.Fatal("descriptor remained lazy or changed client declaration")
	}
	schema := result["inputSchema"].(map[string]any)
	if len(schemaRequired(schema)) != 1 || schema["properties"].(map[string]any)["mode"].(map[string]any)["enum"] == nil {
		t.Fatal("compaction removed structural constraints")
	}
}

func TestAGYToolChoiceEnforcesLatestScopeAndSerialExecution(t *testing.T) {
	tools := []tool{{Type: "function", Name: "read"}, {Type: "function", Name: "write"}}
	for _, test := range []struct {
		choice      string
		read, write bool
	}{{`"none"`, false, false}, {`"auto"`, true, true}, {`{"type":"function","name":"read"}`, true, false}, {`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"write"}]}`, false, true}} {
		p, err := requestToolPolicy(request{ToolChoice: json.RawMessage(test.choice)}, tools)
		if err != nil {
			t.Fatal(err)
		}
		s := &session{tools: tools, policy: p, pending: map[string]*pendingTool{}}
		_, _, read := allowedToolCall(s, callToolName, map[string]any{"name": "read", "arguments": map[string]any{}})
		_, _, write := allowedToolCall(s, tools[1].mcpName(), map[string]any{})
		if read != test.read || write != test.write {
			t.Fatalf("scope bypassed: %s", test.choice)
		}
	}
	serial := false
	p, err := requestToolPolicy(request{Parallel: &serial}, tools)
	if err != nil {
		t.Fatal(err)
	}
	s := &session{tools: tools, policy: p, pending: map[string]*pendingTool{}}
	a, ok := parkToolCall(s, tools[0].mcpName(), map[string]any{})
	if !ok {
		t.Fatal("first serial call denied")
	}
	if _, ok = parkToolCall(s, tools[1].mcpName(), map[string]any{}); ok {
		t.Fatal("serial policy allowed simultaneous calls")
	}
	close(a.Ready)
	if _, ok = parkToolCall(s, tools[1].mcpName(), map[string]any{}); !ok {
		t.Fatal("serial policy blocked a subsequent call")
	}
	if _, err = requestToolPolicy(request{ToolChoice: json.RawMessage(`{"type":"function","name":"missing"}`)}, tools); err == nil {
		t.Fatal("undeclared forced tool accepted")
	}
	native := []tool{{Type: "apply_patch", Name: "client_apply_patch"}}
	forced, err := requestToolPolicy(request{ToolChoice: json.RawMessage(`{"type":"apply_patch"}`)}, native)
	if err != nil || !forced.permits(native[0]) {
		t.Fatal("native tool choice does not match its client adapter identity")
	}
}

func TestAGYFunctionToolSearchUnlocksOnlyDeclaredReferences(t *testing.T) {
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"discover","tools":[{"type":"function","name":"ToolSearch"},{"type":"function","name":"read","defer_loading":true,"parameters":{"type":"object"}}]}`))
	if err != nil || r.Tools[0].Parameters == nil {
		t.Fatal("schema-less client search missing defaults")
	}
	c := &pendingTool{ID: "search", Tool: r.Tools[0], Delivered: true, Ready: make(chan struct{})}
	s := &session{tools: eagerTools(r.Tools), pending: map[string]*pendingTool{c.ID: c}}
	items := []map[string]any{{"type": "function_call_output", "call_id": c.ID, "output": []any{map[string]any{"type": "tool_reference", "tool_name": "read"}}}}
	if err = deliverToolsForRequest(s, items, &r); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := allowedToolCall(s, callToolName, map[string]any{"name": "read", "arguments": map[string]any{}}); !ok {
		t.Fatal("referenced client tool remained deferred")
	}
	if _, err = referencedTools([]any{map[string]any{"type": "tool_reference", "tool_name": "undeclared"}}, r.Tools); err == nil {
		t.Fatal("untrusted reference activated a tool")
	}
}

func TestAGYMCPErrorEnvelopeRetainsImagesAndFailureMarker(t *testing.T) {
	value := map[string]any{"is_error": true, "content": []any{map[string]any{"type": "text", "text": "partial failure"}, map[string]any{"type": "image", "mimeType": "image/png", "data": "YQ=="}}}
	result := mcpOutput(value, "").(map[string]any)
	blocks := result["content"].([]any)
	if result["isError"] != true || len(blocks) != 3 || !strings.Contains(blocks[0].(map[string]string)["text"], "reported failure") || blocks[2].(map[string]string)["type"] != "image" {
		t.Fatal("MCP error envelope lost multimodal result")
	}
}

func TestAGYVeryLargeToolHasSmallDescriptorAndReadableOriginalContract(t *testing.T) {
	props := map[string]any{}
	for i := 0; i < 200; i++ {
		props[fmt.Sprintf("field_%03d", i)] = map[string]any{"type": "string"}
	}
	declared := tool{Type: "function", Name: "large_tool", Parameters: map[string]any{"type": "object", "properties": props}, Description: "full original contract"}
	descriptor := compactMCPDescriptor(map[string]any{"name": declared.mcpName(), "description": declared.key() + ": " + declared.Description, "inputSchema": mcpInputSchema(declared.Parameters)})
	encoded, _ := json.Marshal(descriptor)
	if len(encoded) > 1350 || !strings.Contains(string(encoded), describeToolName) {
		t.Fatal("structurally large schema still exceeds eager loading budget")
	}
	s := &session{secret: "private", tools: []tool{declared}}
	m := &Manager{sessions: map[string]*session{"tenant": s}}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"portal_describe_tool","arguments":{"name":"large_tool"}}}`
	r := httptest.NewRequest("POST", "/mcp/tenant", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer private")
	w := httptest.NewRecorder()
	m.mcp(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "field_199") || !strings.Contains(w.Body.String(), "full original contract") {
		t.Fatal("original contract cannot be retrieved locally")
	}
	if len(s.pending) != 0 {
		t.Fatal("describing a tool started client execution")
	}
	s.tools = nil
	r = httptest.NewRequest("POST", "/mcp/tenant", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer private")
	w = httptest.NewRecorder()
	m.mcp(w, r)
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("revoked original schema still exposed")
	}
	tools := []tool{}
	for i := 0; i < 100; i++ {
		tools = append(tools, toolCopyForLargeTest(i))
	}
	text, err := oversizedToolContext(tools)
	if err != nil || len(text) > MaxText/2 || !strings.Contains(text, describeToolName) {
		t.Fatal("large catalogs are rejected instead of described on demand")
	}
}

func toolCopyForLargeTest(i int) tool {
	return tool{Type: "function", Name: fmt.Sprintf("tool_%d", i), Description: strings.Repeat("large prose ", 200)}
}
