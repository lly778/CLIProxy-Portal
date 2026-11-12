package agy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAGYToolImagesStayMultimodalInMCPAndHistory(t *testing.T) {
	output := []any{
		map[string]any{"type": "input_text", "text": "before"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,YQ=="},
		map[string]any{"type": "image", "mimeType": "image/jpeg", "data": "Yg=="},
		map[string]any{"type": "text", "text": "after"},
	}
	result := mcpOutput(output, "tool update").(map[string]any)["content"].([]any)
	if len(result) != 5 || result[1].(map[string]string)["data"] != "YQ==" || result[1].(map[string]string)["type"] != "image" || result[2].(map[string]string)["mimeType"] != "image/jpeg" || result[4].(map[string]string)["text"] != "tool update" {
		t.Fatalf("MCP image contents lost: %+v", result)
	}
	blocks, err := promptBlocks([]map[string]any{{"type": "function_call_output", "call_id": "fixture", "output": output}})
	if err != nil {
		t.Fatal(err)
	}
	images, text := 0, ""
	for _, raw := range blocks {
		block := raw.(map[string]string)
		if block["type"] == "image" {
			images++
		} else {
			text += block["text"]
		}
	}
	if images != 2 || strings.Contains(text, "YQ==") || !strings.Contains(text, "before") || !strings.Contains(text, "after") {
		t.Fatalf("historical tool images became encoded text: %d %s", images, text)
	}
	for _, raw := range []string{"https://example.test/picture.png", "file:///secret.png", "data:image/svg+xml;base64,YQ==", "data:image/png;base64,!", "data:image/png;base64,"} {
		if _, err := toolResultBlocks([]any{map[string]any{"type": "input_image", "image_url": raw}}); err == nil {
			t.Fatal("unsupported image reference accepted")
		}
	}
}

func TestAGYLongToolRetryReusesOnlyRelayedLiveCall(t *testing.T) {
	fn := tool{Type: "function", Name: "probe"}
	s := &session{tools: []tool{fn}, pending: map[string]*pendingTool{}}
	first, fresh := parkToolCall(s, fn.mcpName(), map[string]any{"command": "fixture"})
	if !fresh || first == nil {
		t.Fatal("initial call not reserved")
	}
	parallel, fresh := parkToolCall(s, fn.mcpName(), map[string]any{"command": "fixture"})
	if !fresh || parallel == first {
		t.Fatal("ordinary identical parallel call was incorrectly deduplicated")
	}
	first.Relayed = true
	retried, fresh := parkToolCall(s, callToolName, map[string]any{"name": "probe", "arguments": map[string]any{"command": "fixture"}})
	if fresh || retried != first || len(s.pending) != 2 {
		t.Fatal("still-running retry would execute the operation twice")
	}
	close(first.Ready)
	again, fresh := parkToolCall(s, fn.mcpName(), map[string]any{"command": "fixture"})
	if !fresh || again == first {
		t.Fatal("completed result was reused for a genuinely new operation")
	}
	s.tools = nil
	if call, _ := parkToolCall(s, fn.mcpName(), map[string]any{"command": "fixture"}); call != nil {
		t.Fatal("revocation bypassed by an old parked operation")
	}
}

func TestAGYDynamicToolUpdateReachesNextNativePromptWithoutNewSession(t *testing.T) {
	var nativePrompt string
	var s *session
	options := []any{map[string]any{"id": "model", "options": []any{map[string]any{"value": "test"}}}}
	p := diskTestProcess(t, func(f frame) any {
		if f.Method == "session/set_config_option" {
			return map[string]any{"configOptions": options}
		}
		if f.Method == "session/prompt" {
			nativePrompt = string(f.Params)
			s.events <- agentEvent{Text: "fixture completed"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	life, cancel := context.WithCancel(t.Context())
	defer cancel()
	var configOptions []configOption
	encoded, _ := json.Marshal(options)
	_ = json.Unmarshal(encoded, &configOptions)
	s = &session{ID: "same-native-session", tenant: "tenant", owner: "owner", model: "agy/test", system: digest(""), tools: []tool{}, advertisedTools: []tool{}, responses: map[string]bool{"previous": true}, pending: map[string]*pendingTool{}, events: make(chan agentEvent, 8), life: life, cancel: cancel, proc: p, options: configOptions, last: time.Now()}
	m := &Manager{cfg: RuntimeConfig{Home: t.TempDir()}, life: life, proc: p, sessions: map[string]*session{"tenant": s}, slot: make(chan struct{}, 1)}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(`{"model":"agy/test","previous_response_id":"previous","input":"use the newly enabled tool","tools":[{"type":"function","name":"new_client_tool","parameters":{"type":"object"}}]}`))
	r.Header.Set("X-Portal-Owner", "owner")
	m.responsesHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"completed"`) || !strings.Contains(nativePrompt, "Client tool configuration update") || !strings.Contains(nativePrompt, "new_client_tool") || s.ID != "same-native-session" {
		t.Fatalf("dynamic update did not reach native prompt: %d %s %s", w.Code, w.Body.String(), nativePrompt)
	}
}

func TestAGYDiskRetainsFrozenCatalogAndLoadedToolAllowlist(t *testing.T) {
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	v := diskTestRecord("dynamic-disk", "owner", time.Now())
	v.AdvertisedTools = []tool{}
	v.Tools = []tool{{Type: "function", Name: "loaded"}}
	v.LoadedTools = v.Tools
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	s, err := d.find("owner", request{Previous: "resp-dynamic-disk"}, nil)
	if err != nil || s == nil || s.advertisedTools == nil || len(s.advertisedTools) != 0 || len(s.loadedTools) != 1 || len(s.tools) != 1 {
		t.Fatalf("disk restore changed the tool catalog: %+v %v", s, err)
	}
}

func TestAGYDynamicToolsValidateBeforeWakingAndEnforceLatestSchema(t *testing.T) {
	old := tool{Type: "function", Name: "probe", Namespace: "functions", Parameters: map[string]any{"type": "object", "properties": map[string]any{"enabled": map[string]any{"type": "boolean", "enum": []any{false}}}}}
	updated := old
	updated.Parameters = map[string]any{"type": "object", "properties": map[string]any{"enabled": map[string]any{"type": "boolean", "enum": []any{true}}}}
	added := tool{Type: "custom", Name: "patch", Namespace: "functions"}
	call := &pendingTool{ID: "call-a", Tool: old, Delivered: true, Ready: make(chan struct{})}
	s := &session{tools: []tool{old}, advertisedTools: []tool{old}, pending: map[string]*pendingTool{call.ID: call}}
	r := request{ToolsProvided: true, Tools: []tool{updated, added}}
	items := []map[string]any{{"type": "function_call_output", "call_id": call.ID, "output": []any{map[string]any{"type": "input_image", "image_url": "https://example.test/image.png"}}}}
	if err := deliverToolsForRequest(s, items, &r); err == nil || digest(s.tools) != digest([]tool{old}) {
		t.Fatal("invalid result changed live tool permissions")
	}
	select {
	case <-call.Ready:
		t.Fatal("invalid image released a parked call")
	default:
	}
	items[0]["output"] = "client completed"
	if err := deliverToolsForRequest(s, items, &r); err != nil {
		t.Fatal(err)
	}
	if len(s.tools) != 2 || len(s.advertisedTools) != 1 || digest(s.advertisedTools[0]) != digest(old) || !strings.Contains(call.Update, "functions.patch") {
		t.Fatal("tool delta changed frozen MCP declarations or lost new tool")
	}
	if _, _, ok := allowedToolCall(s, old.mcpName(), map[string]any{"enabled": false}); ok {
		t.Fatal("old schema remained callable")
	}
	if _, _, ok := allowedToolCall(s, callToolName, map[string]any{"name": "functions.probe", "arguments": map[string]any{"enabled": true}}); !ok {
		t.Fatal("new schema was not callable via helper")
	}
	if got, args, ok := allowedToolCall(s, callToolName, map[string]any{"name": "functions.patch", "arguments": map[string]any{"input": "patch fixture"}}); !ok || got.Type != "custom" || args["input"] != "patch fixture" {
		t.Fatal("new custom tool lost original identity")
	}
	s.tools = nil
	if _, _, ok := allowedToolCall(s, old.mcpName(), map[string]any{"enabled": true}); ok {
		t.Fatal("revoked direct tool remained callable")
	}
	if _, _, ok := allowedToolCall(s, callToolName, map[string]any{"name": "functions.patch", "arguments": map[string]any{"input": "patch fixture"}}); ok {
		t.Fatal("revoked indirect tool remained callable")
	}
}

func TestAGYClientToolSearchLoadsOnlyMatchedResultsAndPreservesSchemas(t *testing.T) {
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"discover","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}},{"type":"function","name":"deferred","defer_loading":true}]}`))
	if err != nil || len(r.Tools) != 2 || len(eagerTools(r.Tools)) != 1 || len(r.UnavailableTools) != 0 {
		t.Fatalf("client tool search unsupported: %+v %v", r, err)
	}
	search := r.Tools[0]
	call := &pendingTool{ID: "search-a", Tool: search, Arguments: map[string]any{"query": "find"}, Delivered: true, Ready: make(chan struct{})}
	s := &session{tools: eagerTools(r.Tools), advertisedTools: eagerTools(r.Tools), pending: map[string]*pendingTool{call.ID: call}}
	var definitions []any
	_ = json.Unmarshal([]byte(`[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"found","defer_loading":true,"parameters":{"type":"object","properties":{"flag":{"type":"boolean","enum":[false]}}}}]}]`), &definitions)
	items := []map[string]any{{"type": "tool_search_output", "execution": "client", "call_id": "foreign-call", "tools": definitions}}
	if err := deliverToolsForRequest(s, items, &r); err == nil || len(s.loadedTools) != 0 {
		t.Fatal("unmatched search activated tools")
	}
	items[0]["call_id"] = call.ID
	if err := deliverToolsForRequest(s, items, &r); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := allowedToolCall(s, callToolName, map[string]any{"name": "functions.found", "arguments": map[string]any{"flag": false}}); !ok {
		t.Fatal("discovered function unavailable")
	}
	if _, _, ok := allowedToolCall(s, callToolName, map[string]any{"name": "functions.found", "arguments": map[string]any{"flag": true}}); ok {
		t.Fatal("discovery bypassed original enum validation")
	}
	if tools, _ := requestedTools(s, request{}); len(tools) != 2 {
		t.Fatal("omitted tools lost previously loaded definitions")
	}
	replay := r
	replay.Items = items
	if tools, err := historicalLoadedTools(replay); err != nil || len(tools) != 0 {
		t.Fatal("an isolated historical output activated tools")
	}
	replay.Items = append([]map[string]any{{"type": "tool_search_call", "execution": "client", "call_id": call.ID}}, items...)
	if tools, err := historicalLoadedTools(replay); err != nil || len(tools) != 1 || tools[0].key() != "functions.found" {
		t.Fatal("paired historical discovery could not be replayed")
	}
	w := httptest.NewRecorder()
	out := newResponse(w, true, "agy/test")
	if err := out.call(call); err != nil {
		t.Fatal(err)
	}
	if err := out.finish(Usage{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"type":"tool_search_call"`) || !strings.Contains(w.Body.String(), `"execution":"client"`) || strings.Contains(w.Body.String(), "custom_tool_call_input") || strings.Contains(w.Body.String(), "function_call_arguments") {
		t.Fatal("wrong Responses tool-search stream format")
	}
}
