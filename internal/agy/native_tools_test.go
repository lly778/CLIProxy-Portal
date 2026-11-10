package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func nativeFixture(t *testing.T, declaration, arguments string) (tool, map[string]any) {
	t.Helper()
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"fixture","tools":[` + declaration + `]}`))
	if err != nil || len(r.Tools) != 1 {
		t.Fatalf("native declaration rejected: %+v %v", r, err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		t.Fatal(err)
	}
	return r.Tools[0], args
}

func TestAGYNativeClientToolsWireFormatAndResults(t *testing.T) {
	for _, tc := range []struct {
		name, declaration, arguments, result, wire string
	}{
		{"shell", `{"type":"shell","environment":{"type":"local"}}`, `{"action":{"commands":["printf fixture"],"max_output_length":4096,"timeout_ms":1000}}`, `{"type":"shell_call_output","call_id":"fixture","max_output_length":4096,"output":[{"stdout":"fixture","stderr":"","outcome":{"type":"exit","exit_code":0}}]}`, "shell_call"},
		{"legacy-shell", `{"type":"local_shell"}`, `{"action":{"type":"exec","command":["echo","fixture"],"env":{}}}`, `{"type":"local_shell_call_output","id":"fixture","output":"fixture","status":"completed"}`, "local_shell_call"},
		{"patch", `{"type":"apply_patch"}`, `{"operation":{"type":"create_file","path":"fixture.txt","diff":"+fixture\n"}}`, `{"type":"apply_patch_call_output","call_id":"fixture","status":"completed","output":"created"}`, "apply_patch_call"},
		{"computer", `{"type":"computer"}`, `{"actions":[{"type":"screenshot"}]}`, `{"type":"computer_call_output","call_id":"fixture","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,YQ=="}}`, "computer_call"},
		{"legacy-computer", `{"type":"computer_use_preview","display_width":640,"display_height":480,"environment":"browser"}`, `{"action":{"type":"click","x":20,"y":30,"button":"left"}}`, `{"type":"computer_call_output","call_id":"fixture","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,YQ=="}}`, "computer_call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn, args := nativeFixture(t, tc.declaration, tc.arguments)
			if fn.Type == "computer" || fn.Type == "computer_use_preview" {
				encoded, _ := json.Marshal(map[string]any{"name": fn.mcpName(), "description": fn.key() + ": " + fn.Description, "inputSchema": nativeComputerMCPSchema(fn)})
				if len(encoded) > 1350 {
					t.Fatal("computer schema exceeds the eager MCP budget")
				}
			}
			s := &session{tools: []tool{fn}, pending: map[string]*pendingTool{}}
			if _, _, allowed := allowedToolCall(s, fn.mcpName(), args); !allowed {
				t.Fatal("valid native tool action rejected")
			}
			call := &pendingTool{ID: "fixture", Tool: fn, Arguments: args, Delivered: true, Ready: make(chan struct{})}
			s.pending[call.ID] = call
			for _, stream := range []bool{false, true} {
				w := httptest.NewRecorder()
				out := newResponse(w, stream, "agy/test")
				if err := out.call(call); err != nil {
					t.Fatal(err)
				}
				if err := out.finish(Usage{}); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(w.Body.String(), `"type":"`+tc.wire+`"`) || strings.Contains(w.Body.String(), `"type":"function_call"`) {
					t.Fatalf("wrong native wire format: %s", w.Body.String())
				}
			}
			wrong := []map[string]any{{"type": "function_call_output", "call_id": call.ID, "output": "wrong executor format"}}
			if err := deliverTools(s, wrong); err == nil {
				t.Fatal("native tool accepted a generic result")
			}
			select {
			case <-call.Ready:
				t.Fatal("invalid result woke native tool")
			default:
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(tc.result), &result); err != nil {
				t.Fatal(err)
			}
			if err := deliverTools(s, []map[string]any{result}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-call.Ready:
			default:
				t.Fatal("valid result did not wake native tool")
			}
			if strings.HasPrefix(tc.name, "computer") || tc.name == "legacy-computer" {
				blocks := mcpOutput(call.Output, "").(map[string]any)["content"].([]any)
				if blocks[0].(map[string]string)["type"] != "image" {
					t.Fatal("computer screenshot became text")
				}
			}
			s.tools = nil
			if _, _, allowed := allowedToolCall(s, fn.mcpName(), args); allowed {
				t.Fatal("undeclared native executor remained callable")
			}
		})
	}
}

func TestAGYNativeToolsRejectMalformedAndHostedExecutors(t *testing.T) {
	for _, declaration := range []string{`{"type":"shell"}`, `{"type":"shell","environment":{"type":"container_auto"}}`, `{"type":"computer_use_preview","display_width":0,"display_height":480}`} {
		if _, err := parseRequest([]byte(`{"model":"agy/test","input":"fixture","tools":[` + declaration + `]}`)); err == nil {
			t.Fatal("missing or hosted executor accepted")
		}
	}
	for _, tc := range []struct{ declaration, args string }{
		{`{"type":"shell","environment":{"type":"local"}}`, `{"action":{"commands":[]}}`},
		{`{"type":"shell","environment":{"type":"local"}}`, `{"action":{"commands":["echo"],"timeout_ms":-1}}`},
		{`{"type":"local_shell"}`, `{"action":{"type":"exec","command":["echo"],"env":{"INVALID":123}}}`},
		{`{"type":"apply_patch"}`, `{"operation":{"type":"update_file","path":"fixture.txt"}}`},
		{`{"type":"apply_patch"}`, `{"operation":{"type":"delete_file","path":"fixture.txt","diff":"not applicable"}}`},
		{`{"type":"computer"}`, `{"actions":[{"type":"exec","command":"not a UI action"}]}`},
		{`{"type":"computer"}`, `{"actions":[{"type":"click","x":-1,"y":0,"button":"left"}]}`},
		{`{"type":"computer_use_preview","display_width":640,"display_height":480}`, `{"action":{"type":"click","x":640,"y":10,"button":"left"}}`},
	} {
		fn, args := nativeFixture(t, tc.declaration, tc.args)
		if nativeArgumentsValid(fn, args) {
			t.Fatalf("invalid native action accepted: %s", tc.args)
		}
	}
}

func TestAGYNativePatchFailureAndScreenshotValidation(t *testing.T) {
	fn, args := nativeFixture(t, `{"type":"apply_patch"}`, `{"operation":{"type":"delete_file","path":"fixture.txt"}}`)
	call := &pendingTool{ID: "fixture", Tool: fn, Arguments: args}
	value, err := nativeResultValue(call, map[string]any{"type": "apply_patch_call_output", "status": "failed", "output": "file does not exist"})
	if err != nil || mcpOutput(value, "").(map[string]any)["isError"] != true {
		t.Fatal("patch failure was not returned as a tool error")
	}
	computer, _ := nativeFixture(t, `{"type":"computer"}`, `{"actions":[{"type":"screenshot"}]}`)
	call.Tool = computer
	if _, err := nativeResultValue(call, map[string]any{"type": "computer_call_output", "output": map[string]any{"type": "computer_screenshot", "image_url": "https://example.test/image.png"}}); err == nil {
		t.Fatal("remote screenshot URL accepted")
	}
}
