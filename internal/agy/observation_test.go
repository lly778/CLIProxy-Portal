package agy

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type observationLog struct {
	mu     sync.Mutex
	events []Observation
}

func (l *observationLog) save(v Observation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, v)
	return nil
}
func (l *observationLog) snapshot() []Observation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Observation(nil), l.events...)
}

func TestSummaryObservationAccountsRealNativeStepsAndCacheReuse(t *testing.T) {
	var m *Manager
	var calls int
	m, _ = recoveryFixture(t, func(s *session, _ frame) any {
		calls++
		dir := filepath.Join(m.cfg.Home, "agy", "conversations")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Error(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(dir, s.ID+".db"))
		if err != nil {
			t.Error(err)
			return nil
		}
		defer db.Close()
		_, err = db.Exec(`CREATE TABLE steps(idx INTEGER PRIMARY KEY,step_type INTEGER,status INTEGER,metadata BLOB,step_payload BLOB)`)
		if err != nil {
			t.Error(err)
		}
		_, err = db.Exec(`INSERT INTO steps(idx,step_type,status,metadata) VALUES(0,15,3,?)`, usageProto(2, 100, 3, 20))
		if err != nil {
			t.Error(err)
		}
		s.events <- agentEvent{Text: "checkpoint"}
		return map[string]any{"stopReason": "end_turn"}
	})
	log := &observationLog{}
	m.cfg.Observe = log.save
	r := httptest.NewRequest("POST", "/internal/compact", nil)
	r.Header.Set("X-Portal-Request-ID", "origin-request")
	r, _ = m.traceRequest(r, "real-owner:key", "agy/test")
	blocks := []any{map[string]string{"type": "text", "text": strings.Repeat("x", 2*compactChunk+10)}}
	for i := 0; i < 2; i++ {
		if _, err := m.summarizeHistory(r.Context(), "real-owner:key", "agy/test", blocks, nil); err != nil {
			t.Fatal(err)
		}
	}
	var summaries, hits, steps int
	var total int64
	for _, v := range log.snapshot() {
		if v.Kind == "summary_cache" {
			hits++
		}
		if v.Kind != "summary_usage" {
			continue
		}
		summaries++
		steps += v.ModelCalls
		total += v.Usage.Total
		if v.Owner != "real-owner:key" || v.RequestID != "origin-request" || v.SessionID == "" || !v.Usage.Known {
			t.Fatalf("summary lost billing identity/usage: %+v", v)
		}
	}
	if calls != 3 || summaries != 3 || steps != 3 || total != 360 || hits != 3 {
		t.Fatalf("calls=%d summaries=%d steps=%d total=%d hits=%d", calls, summaries, steps, total, hits)
	}
}

func TestToolReceiptsRequireValidatedResultsAndDoNotMergeIdenticalActions(t *testing.T) {
	log := &observationLog{}
	m := &Manager{cfg: RuntimeConfig{Observe: log.save}}
	trace := &requestTrace{m: m, owner: "owner:key", request: "r", model: "agy/test", session: "native"}
	fn := tool{Type: "function", Name: "write", Parameters: map[string]any{"type": "object"}}
	s := &session{ID: "native", trace: trace, tools: []tool{fn}, pending: map[string]*pendingTool{}}
	a, first := parkToolCall(s, fn.mcpName(), map[string]any{})
	b, second := parkToolCall(s, fn.mcpName(), map[string]any{})
	if !first || !second || a.ID == b.ID {
		t.Fatal("identical independent actions were merged")
	}
	a.Delivered, b.Delivered = true, true
	items := []map[string]any{{"type": "function_call_output", "call_id": a.ID, "output": `{"isError":false,"content":[{"type":"text","text":"sensitive-result"}]}`}}
	if deliverTools(s, items) == nil {
		t.Fatal("partial batch accepted")
	}
	var issued int
	for _, v := range log.snapshot() {
		if v.Kind == "tool" {
			issued++
		}
	}
	if issued != 2 {
		t.Fatal("invalid batch created receipts")
	}
	items = append(items, map[string]any{"type": "function_call_output", "call_id": b.ID, "output": `{"isError":true,"content":[{"type":"text","text":"failed"}]}`})
	if err := deliverTools(s, items); err != nil {
		t.Fatal(err)
	}
	if err := deliverTools(s, items); err != nil {
		t.Fatal(err)
	}
	var events []Observation
	for _, v := range log.snapshot() {
		if v.Kind == "tool" {
			events = append(events, v)
		}
	}
	if len(events) != 4 {
		t.Fatal("identical result replay duplicated receipt", len(events))
	}
	for _, v := range events {
		if v.State == "issued" && (v.Outcome != "unknown" || v.ResultHash != "") {
			t.Fatal("generation counted as execution")
		}
		if v.State == "result_received" && ((v.CallID == a.ID && v.Outcome != "success") || (v.CallID == b.ID && v.Outcome != "failed")) {
			t.Fatal("explicit outcome lost", v)
		}
		data, _ := json.Marshal(v)
		if strings.Contains(string(data), "sensitive-result") {
			t.Fatal("raw client result entered metadata")
		}
	}
	if receiptOutcome("saved successfully") != "unknown" || receiptOutcome(`{"status":"completed"}`) != "unknown" {
		t.Fatal("free-form result became proof of execution")
	}
}

func TestReplayRecordsDeliveryWithoutExecutingAgain(t *testing.T) {
	log := &observationLog{}
	m := &Manager{life: t.Context(), cfg: RuntimeConfig{Observe: log.save}}
	defer m.stopResponseReplays(false)
	calls := 0
	execute := func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, trace := m.traceRequest(r, "owner:key", "agy/test")
		trace.emit(Observation{Kind: "summary_usage", State: "completed", ModelCalls: 1})
		_, _ = w.Write([]byte(`{"id":"same-response","status":"completed","output":[]}`))
	}
	for i := 0; i < 2; i++ {
		r := replayTestRequest(t.Context(), `{"model":"agy/test","input":"hello"}`, "stable")
		m.replayResponse(httptest.NewRecorder(), r, execute)
	}
	var summary, replay int
	for _, v := range log.snapshot() {
		if v.Kind == "summary_usage" {
			summary++
		}
		if v.Kind == "http_replay" {
			replay++
		}
	}
	if calls != 1 || summary != 1 || replay != 1 {
		t.Fatalf("executions=%d summary=%d replay=%d", calls, summary, replay)
	}
}

func TestSummaryFailureKeepsConfirmedUsage(t *testing.T) {
	// A failed ACP result still incurred a completed native generation.
	var m *Manager
	m, _ = recoveryFixture(t, func(s *session, _ frame) any {
		dir := filepath.Join(m.cfg.Home, "agy", "conversations")
		_ = os.MkdirAll(dir, 0700)
		db, err := sql.Open("sqlite", filepath.Join(dir, s.ID+".db"))
		if err != nil {
			t.Error(err)
			return nil
		}
		defer db.Close()
		_, err = db.Exec(`CREATE TABLE steps(idx INTEGER PRIMARY KEY,step_type INTEGER,status INTEGER,metadata BLOB); INSERT INTO steps VALUES(0,15,3,?)`, usageProto(2, 50, 3, 10))
		if err != nil {
			t.Error(err)
		}
		return map[string]any{"stopReason": "cancelled"}
	})
	log := &observationLog{}
	m.cfg.Observe = log.save
	trace := &requestTrace{m: m, owner: "owner:key", request: "r", model: "agy/test"}
	ctx := context.WithValue(t.Context(), traceKey{}, trace)
	if _, err := m.summaryTurn(ctx, "agy/test", nil, nil); err == nil {
		t.Fatal("failed summary accepted")
	}
	var found bool
	for _, v := range log.snapshot() {
		if v.Kind == "summary_usage" {
			found = true
			if v.State != "failed" || v.Usage.Total != 60 || v.ModelCalls != 1 {
				t.Fatal(v)
			}
		}
	}
	if !found {
		t.Fatal("failed summary vanished from ledger")
	}
}

func TestMCPResultReceiptIsDeliveredOnceAndAwaitIsSeparate(t *testing.T) {
	log := &observationLog{}
	m := &Manager{cfg: RuntimeConfig{Observe: log.save}, sessions: map[string]*session{}}
	trace := &requestTrace{m: m, owner: "owner:key", request: "original", model: "agy/test", session: "native"}
	fn := tool{Type: "function", Name: "write", Parameters: map[string]any{"type": "object"}}
	s := &session{ID: "native", tenant: "tenant", secret: strings.Repeat("s", 32), trace: trace, tools: []tool{fn}, pending: map[string]*pendingTool{}, running: true, life: t.Context(), events: make(chan agentEvent, 1)}
	m.sessions[s.tenant] = s
	request := func(name string, args map[string]any) *http.Request {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "rpc", "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		r := httptest.NewRequest("POST", "/mcp/tenant", strings.NewReader(string(data)))
		r.Header.Set("Authorization", "Bearer "+s.secret)
		return r
	}
	done := make(chan struct{})
	go func() { defer close(done); m.mcp(httptest.NewRecorder(), request(fn.mcpName(), map[string]any{})) }()
	var call *pendingTool
	select {
	case event := <-s.events:
		call = event.Call
	case <-time.After(time.Second):
		t.Fatal("no client tool")
	}
	s.mu.Lock()
	call.Delivered = true
	s.mu.Unlock()
	if err := deliverTools(s, []map[string]any{{"type": "function_call_output", "call_id": call.ID, "output": `{"isError":false}`}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("result not delivered")
	}
	m.mcp(httptest.NewRecorder(), request(awaitName, map[string]any{"call_id": call.ID}))
	states := map[string]int{}
	for _, v := range log.snapshot() {
		states[v.State]++
	}
	if states["issued"] != 1 || states["result_received"] != 1 || states["result_delivered"] != 1 || states["await"] != 1 {
		t.Fatal("await replay changed execution receipts", states)
	}
}

func TestWaitingSnapshotRetainsReceiptOrigin(t *testing.T) {
	disk, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.db.Close()
	c := &pendingTool{ID: "call-original", traceRequest: "original-request", traceSession: "original-session", Delivered: true, Ready: make(chan struct{}), Tool: tool{Type: "function", Name: "write"}}
	s := &session{ID: "original-session", owner: "owner:key", model: "agy/test", last: time.Now(), responses: map[string]bool{}, pending: map[string]*pendingTool{c.ID: c}}
	if err := disk.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	loaded, err := disk.loadSession(s.ID, s.owner, 0)
	if err != nil || loaded == nil {
		t.Fatal("cannot restore receipt origin", err)
	}
	got := loaded.pending[c.ID]
	if got == nil || got.traceRequest != "original-request" || got.traceSession != "original-session" {
		t.Fatal("snapshot lost original receipt identity")
	}
}

func TestSummaryMissingNativeUsageStaysUnknown(t *testing.T) {
	m, _ := recoveryFixture(t, func(s *session, _ frame) any {
		s.events <- agentEvent{Text: "checkpoint"}
		return map[string]any{"stopReason": "end_turn"}
	})
	log := &observationLog{}
	m.cfg.Observe = log.save
	trace := &requestTrace{m: m, owner: "owner:key", request: "r", model: "agy/test"}
	if _, err := m.summaryTurn(context.WithValue(t.Context(), traceKey{}, trace), "agy/test", nil, nil); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range log.snapshot() {
		if v.Kind == "summary_usage" {
			found = true
			if v.Usage.Known || v.ModelCalls != 0 {
				t.Fatal("missing usage fabricated a model count/known zero", v)
			}
		}
	}
	if !found {
		t.Fatal("unknown summary omitted")
	}
}

func TestObservationIdentityUsesOnlyPrivateContext(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := ""
		if r.URL.Path == "/internal/responses" {
			want = "trusted-timing"
		}
		if r.Header.Get("X-Portal-Request-ID") != want {
			t.Error("untrusted observation identity forwarded")
		}
		w.WriteHeader(204)
	}))
	defer worker.Close()
	client, err := NewClient(worker.URL, strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	headers := make(http.Header)
	headers.Set("X-Portal-Request-ID", "forged")
	ctx := WithObservationRequest(WithRequestIdentity(t.Context(), headers), "trusted-timing")
	for _, path := range []string{"/internal/responses", "/internal/status"} {
		r, err := client.Request(ctx, "POST", path, "owner:key", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
}
