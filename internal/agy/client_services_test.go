package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAGYClientServiceDeclarationFormatsPreserveExecutorAndSchema(t *testing.T) {
	for _, decl := range []string{
		`{"type":"function","name":"FileLookup","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}`,
		`{"type":"function","function":{"name":"FileLookup","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}`,
		`{"name":"FileLookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}`,
		`{"name":"FileLookup","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}`,
	} {
		r, err := parseRequest([]byte(`{"model":"agy/test","input":"search","tools":[` + decl + `]}`))
		if err != nil || len(r.Tools) != 1 || r.Tools[0].Type != "function" || r.Tools[0].Name != "FileLookup" {
			t.Fatalf("client declaration failed: %v %s", err, decl)
		}
		if !validArguments(r.Tools[0].Parameters, map[string]any{"query": "test"}, 0) || validArguments(r.Tools[0].Parameters, map[string]any{"query": false}, 0) {
			t.Fatal("client schema lost")
		}
	}
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"search","tools":[{"type":"namespace","name":"apps","tools":[{"type":"namespace","name":"files","tools":[{"name":"lookup","inputSchema":{"type":"object"}}]}]},{"type":"mcp","execution":"client","tools":[{"name":"mcp__files__search_files","inputSchema":{"type":"object"}}]}]}`))
	if err != nil || len(r.Tools) != 2 || r.Tools[0].key() != "apps.files.lookup" || r.Tools[1].key() != "mcp__files__search_files" {
		t.Fatalf("client MCP identities changed: %+v %v", r.Tools, err)
	}
	_, err = parseRequest([]byte(`{"model":"agy/test","input":"search","tools":[{"type":"namespace","name":"outer","tools":[{"type":"function","name":"probe","namespace":"different"}]}]}`))
	if err == nil {
		t.Fatal("conflicting executor namespace accepted")
	}
}

func TestAGYOfficialServicesReplaceLegacyClientExecutors(t *testing.T) {
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"search","tools":[{"type":"web_search"},{"type":"image_generation"},{"type":"function","name":"mcp__codex_apps__search_service_web_run","parameters":{"type":"object"}},{"type":"tool_search","execution":"client"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	system, _ := systemText(r)
	if len(r.UnavailableTools) != 0 || len(r.Tools) != 1 || r.Tools[0].Type != "tool_search" {
		t.Fatal("legacy search was not removed or official service was marked unavailable")
	}
	capabilities := clientCapabilityText(r.Tools)
	for _, want := range []string{"tool_discovery is available", "tool-discovery", "not the web"} {
		if !strings.Contains(capabilities, want) {
			t.Fatalf("missing capability guidance %s", want)
		}
	}
	before := digest(system)
	r.Tools = append(r.Tools, tool{Type: "function", Name: "run_python"})
	after, _ := systemText(r)
	if digest(after) != before {
		t.Fatal("client tool updates change system identity and force native rebuild")
	}
	delta, err := toolDelta(r.Tools[:len(r.Tools)-1], r.Tools)
	if err != nil || !strings.Contains(delta, "code_execution is available") {
		t.Fatal("dynamic service activation not explained")
	}
	r.Tools = []tool{{Type: "function", Name: "search_files", Deferred: true}}
	if strings.Contains(clientCapabilityText(r.Tools), "is available") {
		t.Fatal("deferred search advertised before discovery")
	}
}

func TestAGYClientServicesUseActualFunctionRoundTrips(t *testing.T) {
	for _, name := range []string{"search_files", "run_python", "mcp__remote__lookup", "exec_command"} {
		t.Run(name, func(t *testing.T) {
			decl := tool{Type: "function", Name: name, Namespace: "functions", Parameters: map[string]any{"type": "object"}}
			s := &session{tools: []tool{decl}, pending: map[string]*pendingTool{}}
			call, fresh := parkToolCall(s, decl.mcpName(), map[string]any{})
			if !fresh {
				t.Fatal("client service not callable")
			}
			w := httptest.NewRecorder()
			out := newResponse(w, false, "agy/test")
			if err := out.call(call); err != nil {
				t.Fatal(err)
			}
			if err := out.finish(Usage{}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(w.Body.String(), `"type":"function_call"`) || !strings.Contains(w.Body.String(), `"name":"`+name+`"`) || !strings.Contains(w.Body.String(), `"namespace":"functions"`) {
				t.Fatal("executor wire identity changed")
			}
			call.Delivered = true
			if err := deliverTools(s, []map[string]any{{"type": "function_call_output", "call_id": call.ID, "output": map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": "actual client fixture result"}}}}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-call.Ready:
			default:
				t.Fatal("actual result not released")
			}
		})
	}
}

func TestAGYClientMCPResultsRetainImagesStructuredDataAndResourceReferences(t *testing.T) {
	value := map[string]any{"isError": false, "structuredContent": map[string]any{"total": 2}, "content": []any{
		map[string]any{"type": "search_result", "title": "Found document", "source": "https://example.test/doc", "content": []any{map[string]any{"type": "text", "text": "search matched"}}},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "YQ=="}},
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///client/report.txt", "mimeType": "text/plain", "text": "retrieved content"}},
		map[string]any{"type": "resource_link", "uri": "https://example.test/chart.png", "name": "Python chart", "mimeType": "image/png"},
	}}
	encoded, _ := json.Marshal(value)
	blocks, err := toolResultBlocks(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	all, _ := json.Marshal(blocks)
	text := string(all)
	for _, want := range []string{"search matched", "https://example.test/doc", "retrieved content", "Python chart", "structured result", "\"total\":2"} {
		if !strings.Contains(text, want) && !strings.Contains(text, strings.ReplaceAll(want, `"`, `\"`)) {
			t.Fatalf("result lost %q: %s", want, text)
		}
	}
	images := 0
	for _, raw := range blocks {
		if raw.(map[string]string)["type"] == "image" {
			images++
		}
	}
	if images != 1 {
		t.Fatal("client-generated image remained serialized text")
	}
	link, err := toolResultBlocks(map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/generated.png"}})
	if err != nil || link[0].(map[string]string)["type"] != "text" || !strings.Contains(link[0].(map[string]string)["text"], "pixels not loaded") {
		t.Fatal("remote artifact link was misrepresented as actual pixels")
	}
}

func TestAGYDocumentsAndPreviousHostedCallsAreContextOnly(t *testing.T) {
	items := []map[string]any{
		{"type": "web_search_call", "id": "past-search", "action": map[string]any{"type": "search", "query": "old query"}},
		{"type": "mcp_call", "name": "lookup", "server_label": "old-mcp", "output": "previous remote result"},
		{"type": "image_generation_call", "result": "YQ==", "output_format": "png"},
		{"role": "user", "content": []any{map[string]any{"type": "document", "title": "report", "source": map[string]any{"type": "text", "data": "provided document text"}}, map[string]any{"type": "search_result", "source": "https://example.test/", "content": []any{map[string]any{"type": "text", "text": "prior search evidence"}}}}},
	}
	blocks, err := promptBlocks(items)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(blocks)
	for _, want := range []string{"context only, do not execute again", "previous remote result", "provided document text", "prior search evidence"} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("history compatibility missing %s", want)
		}
	}
	if _, _, err := clientContentBlock(map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "YQ=="}}, 0); err == nil {
		t.Fatal("binary PDF pretended to be extracted text")
	}
	var nested any = map[string]any{"type": "text", "text": "end"}
	for i := 0; i < 20; i++ {
		nested = map[string]any{"type": "search_result", "content": []any{nested}}
	}
	if _, err := toolResultBlocks(nested); err == nil {
		t.Fatal("unbounded recursive result accepted")
	}
	for _, decl := range []string{`{"type":"mcp","server_label":"remote","server_url":"https://example.test/mcp"}`, `{"type":"file_search"}`, `{"type":"code_interpreter"}`} {
		r, err := parseRequest([]byte(`{"model":"agy/test","input":"test","tools":[` + decl + `]}`))
		if err != nil || len(r.Tools) != 0 || len(r.UnavailableTools) != 1 {
			t.Fatal("a declaration manufactured an executor")
		}
	}
}
