package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func localMCPCall(t *testing.T, m *Manager, s *session, method string, params any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "fixture-request", "method": method, "params": params})
	r := httptest.NewRequest("POST", "/mcp/"+s.tenant, strings.NewReader(string(data)))
	r.Header.Set("Authorization", "Bearer "+s.secret)
	w := httptest.NewRecorder()
	m.mcp(w, r)
	var reply map[string]any
	if w.Code != 200 || unmarshalExact(w.Body.Bytes(), &reply) != nil {
		t.Fatalf("local MCP failed: %d %.1000s", w.Code, w.Body.String())
	}
	result, _ := reply["result"].(map[string]any)
	return result
}

func TestContractCacheRevisionRevocationAndOriginalSchemaValidation(t *testing.T) {
	log := &observationLog{}
	m, s := recoveryFixture(t, func(*session, frame) any { t.Error("contract read invoked a model"); return nil })
	m.cfg.Observe = log.save
	s.secret = "fixture-secret"
	s.trace = &requestTrace{m: m, owner: s.owner, model: s.model, request: "contract"}
	var r request
	parsed, err := parseRequest([]byte(`{"model":"agy/test","input":"test","tools":[{"type":"function","name":"write","namespace":"project","parameters":{"type":"object","properties":{"record":{"const":9007199254740993}},"required":["record"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r = parsed
	s.tools, s.toolHash = r.Tools, digest(r.Tools)
	args := map[string]any{"name": "project.write"}
	a := localMCPCall(t, m, s, "tools/call", map[string]any{"name": describeToolName, "arguments": args})
	b := localMCPCall(t, m, s, "tools/call", map[string]any{"name": describeToolName, "arguments": args})
	if digest(a) != digest(b) || len(s.pending) != 0 {
		t.Fatal("description changed or executed an action")
	}
	encoded, _ := json.Marshal(a)
	if !strings.Contains(string(encoded), "9007199254740993") || !strings.Contains(string(encoded), "projection_version") {
		t.Fatal("original contract or version missing")
	}
	if c, _ := parkToolCall(s, callToolName, map[string]any{"name": "project.write", "arguments": map[string]any{"record": json.Number("9007199254740992")}}); c != nil {
		t.Fatal("projection bypassed exact original constraint")
	}
	c, fresh := parkToolCall(s, callToolName, map[string]any{"name": "project.write", "arguments": map[string]any{"record": json.Number("9007199254740993")}})
	if !fresh || c.Route != "portal_call_tool" {
		t.Fatal("authorized contract failed")
	}
	updated := s.tools[0]
	updated.Description = "updated contract"
	s.tools, s.toolHash = []tool{updated}, digest([]tool{updated})
	changed := localMCPCall(t, m, s, "tools/call", map[string]any{"name": describeToolName, "arguments": args})
	if digest(changed) == digest(a) {
		t.Fatal("stale cache reused after revision")
	}
	s.tools = nil
	if !toolResultFailed(localMCPCall(t, m, s, "tools/call", map[string]any{"name": describeToolName, "arguments": args})) {
		t.Fatal("revoked cached contract remained readable")
	}
	if c, _ := parkToolCall(s, callToolName, map[string]any{"name": "project.write", "arguments": map[string]any{}}); c != nil {
		t.Fatal("revoked indirect tool executed")
	}
	var hits, misses, rejects int
	for _, event := range log.snapshot() {
		if event.Kind == "contract_cache" {
			if event.State == "hit" {
				hits++
			} else {
				misses++
			}
		}
		if event.Kind == "tool_rejected" {
			rejects++
		}
	}
	if hits != 1 || misses != 2 || rejects != 2 {
		t.Fatal("cache/rejection telemetry incorrect", hits, misses, rejects)
	}
	c.Delivered = true
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	if err = d.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	restored, err := d.loadSession(s.ID, s.owner, 0)
	if err != nil || restored == nil || restored.pending[c.ID] == nil || restored.pending[c.ID].ProjectionVersion != schemaProjectionVersion || restored.pending[c.ID].Route != "portal_call_tool" {
		t.Fatal("waiting contract provenance lost across recovery", err)
	}
}

func TestMCPInitializeDoesNotAdvertiseUnverifiedDynamicTransport(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.secret = "fixture-secret"
	s.proc.evidence = newRuntimeEvidence("fixture-hash", "agy", "", 1, nil)
	result := localMCPCall(t, m, s, "initialize", map[string]any{})
	caps := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"].(map[string]any)["listChanged"]; ok {
		t.Fatal("unverified refresh advertised")
	}
	e := s.proc.runtimeEvidence()
	if e.Capabilities["mcp_http"].Source != "observed_protocol_success" || e.Capabilities["mcp_over_acp"].State != "unknown" {
		t.Fatal("unverified transport capability promoted", e)
	}
}
