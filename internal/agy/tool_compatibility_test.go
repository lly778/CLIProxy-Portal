package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAGYCodexOptionalServicesDoNotBlockClientToolsAndAreDisclosed(t *testing.T) {
	r, err := parseRequest([]byte(`{"model":"agy/gemini","input":"test","tools":[{"type":"function","name":"shell_command"},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"apply_patch"}]},{"type":"web_search"},{"type":"tool_search","execution":"client"},{"type":"image_generation"},{"type":"file_search"}]}`))
	if err != nil || len(r.Tools) != 3 || len(r.UnavailableTools) != 1 || r.Tools[2].Type != "tool_search" {
		t.Fatalf("Codex declarations rejected or client tools dropped: %+v %v", r, err)
	}
	system, err := systemText(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"file_search"} {
		if !strings.Contains(system, kind) {
			t.Fatalf("agent was not told capability is unavailable: %s", kind)
		}
	}
	w := httptest.NewRecorder()
	out := newResponse(w, false, r.Model)
	out.unavailableTools = r.UnavailableTools
	if err = out.finish(Usage{}); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Metadata map[string]string `json:"metadata"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil || !strings.Contains(reply.Metadata["portal_unavailable_tools"], "file_search") || strings.Contains(reply.Metadata["portal_unavailable_tools"], "web_search") {
		t.Fatal("API capability warning missing")
	}
	r, err = parseRequest([]byte(`{"model":"agy/gemini","input":"test","tools":[{"type":"function","name":"shell_command"},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"apply_patch"}]}]}`))
	if err != nil || len(r.Tools) != 2 || r.Tools[1].Namespace != "functions" {
		t.Fatalf("client tools lost: %+v %v", r.Tools, err)
	}
	for _, tc := range []struct{ tools, code string }{
		{`[{"type":"function"}]`, "invalid_tool_name"},
		{`[{"type":"function","name":"same"},{"type":"function","name":"same"}]`, "duplicate_tool"},
		{`[{"type":"shell","environment":{"type":"container_auto"}}]`, "unsupported_shell_environment"},
		{`[{"type":"unrecognized"}]`, "unsupported_tool"},
	} {
		_, err := parseRequest([]byte(`{"model":"agy/gemini","input":"test","tools":` + tc.tools + `}`))
		e, ok := err.(*Error)
		if !ok || e.Code != tc.code {
			t.Fatalf("wrong diagnostic: %v", err)
		}
	}
}
