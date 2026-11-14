package agy

import (
	"encoding/json"
	"strings"
	"testing"
)

const serverCatalogFixture = `[{"type":"tool_search","execution":"server"},{"type":"function","name":"search_files","namespace":"project","description":"Search client project files","defer_loading":true,"parameters":{"type":"object","properties":{"root":{"type":"string"},"record":{"const":9007199254740993}},"required":["root","record"]}},{"type":"function","name":"write","namespace":"other","description":"Write unrelated data","defer_loading":true,"parameters":{"type":"object"}}]`

func serverCatalogRequest(t *testing.T) request {
	t.Helper()
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"search project","tools":` + serverCatalogFixture + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestServerSearchProducesServerHistoryWithoutDispatchingClientAction(t *testing.T) {
	var m *Manager
	var searchResult map[string]any
	m, s := recoveryFixture(t, func(s *session, frame frame) any {
		searchResult = localMCPCall(t, m, s, "tools/call", map[string]any{"name": callToolName, "arguments": map[string]any{"name": "server_tool_search", "arguments": map[string]any{"query": "search project", "max_results": json.Number("1")}}})
		s.events <- agentEvent{Text: "found client search contract"}
		return map[string]any{"stopReason": "end_turn"}
	})
	s.secret = "fixture-secret"
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"search project","tools":`+serverCatalogFixture+`}`)
	if w.Code != 200 || toolResultFailed(searchResult) {
		t.Fatalf("server search failed: %d %.1500s", w.Code, w.Body.String())
	}
	var result struct {
		Output []map[string]any `json:"output"`
	}
	if unmarshalExact(w.Body.Bytes(), &result) != nil || len(result.Output) != 3 {
		t.Fatalf("server search history missing: %.1500s", w.Body.String())
	}
	if result.Output[0]["type"] != "tool_search_call" || result.Output[1]["type"] != "tool_search_output" || result.Output[0]["execution"] != "server" || result.Output[1]["execution"] != "server" || len(s.pending) != 0 {
		t.Fatal("search pretended to execute a client action")
	}
	if _, err := inspectToolHistory(result.Output); err != nil {
		t.Fatal("search history cannot be compacted", err)
	}
	r := serverCatalogRequest(t)
	r.Items = result.Output
	loaded, err := historicalLoadedTools(r)
	if err != nil || len(loaded) != 1 || loaded[0].key() != "project.search_files" {
		t.Fatal("explicit server provenance did not recover", err)
	}
	args := map[string]any{"root": `D:\client project\资料`, "record": json.Number("9007199254740993")}
	call, fresh := parkToolCall(s, callToolName, map[string]any{"name": "project.search_files", "arguments": args})
	if !fresh || call == nil || digest(call.Arguments) != digest(args) {
		t.Fatal("actual search did not dispatch unchanged to client")
	}
	// A declared search origin is not authority for a different contract version.
	for i := range r.Tools {
		if r.Tools[i].Name == "search_files" {
			r.Tools[i].Description = "changed current version"
		}
	}
	if _, err = historicalLoadedTools(r); err == nil {
		t.Fatal("old server result reactivated a changed contract")
	}
	for _, choice := range []string{`{"type":"tool_search"}`, `{"type":"allowed_tools","mode":"auto","tools":[{"type":"tool_search"}]}`} {
		r.ToolChoice = json.RawMessage(choice)
		p, err := requestToolPolicy(r, eagerTools(r.Tools))
		if err != nil || !p.Names["server_tool_search"] {
			t.Fatal("server tool choice lost execution identity", err)
		}
	}
}

func TestServerSearchCatalogRevocationAndDiskRecovery(t *testing.T) {
	r := serverCatalogRequest(t)
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.tools, s.advertisedTools, s.deferredTools = eagerTools(r.Tools), eagerTools(r.Tools), deferredCatalog(r.Tools)
	s.secret = "fixture-secret"
	s.running = true
	result, event := serverToolSearch(s, s.tools[0], map[string]any{"query": "search project", "max_results": json.Number("1")})
	if toolResultFailed(result) || event == nil {
		t.Fatal("authorized search failed")
	}
	s.running = false
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	if err = d.put(snapshotSession(s), true); err != nil {
		t.Fatal(err)
	}
	restored, err := d.loadSession(s.ID, s.owner, 0)
	if err != nil || len(restored.deferredTools) != 2 || len(restored.loadedTools) != 1 {
		t.Fatal("saved discovery catalog lost", err)
	}
	// An explicit catalog replacing the old one must revoke loaded definitions.
	r.Tools = r.Tools[:1]
	base, loaded := requestedTools(restored, r)
	restored.tools, restored.loadedTools = base, loaded
	if len(loaded) != 0 {
		t.Fatal("revoked search result remained loaded")
	}
	if c, _ := parkToolCall(restored, callToolName, map[string]any{"name": "project.search_files", "arguments": map[string]any{}}); c != nil {
		t.Fatal("revoked discovered tool still executable")
	}
	// Read-only search failure must not activate or dispatch any client tool.
	before := digest(s.tools)
	for _, args := range []map[string]any{{"query": ""}, {"query": "project", "max_results": json.Number("17")}} {
		res, ev := serverToolSearch(s, s.tools[0], args)
		if !toolResultFailed(res) || ev != nil || digest(s.tools) != before {
			t.Fatal("invalid discovery mutated state")
		}
	}
	m.cfg.Secret = strings.Repeat("s", 64)
	if _, err := parseRequest([]byte(`{"model":"agy/test","input":"x","tools":[{"type":"tool_search","execution":"server"},{"type":"tool_search","execution":"client"}]}`)); err == nil {
		t.Fatal("mixed discovery origins accepted")
	}
}
