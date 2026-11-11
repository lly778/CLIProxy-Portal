package agy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T, onPrompt func(*session, frame) any) (*Manager, *session) {
	t.Helper()
	life, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var s *session
	var m *Manager
	var options []configOption
	_ = json.Unmarshal([]byte(`[{"id":"model","options":[{"value":"test"}]}]`), &options)
	p := diskTestProcess(t, func(f frame) any {
		if f.Method == "session/prompt" {
			var params struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(f.Params, &params)
			m.mu.Lock()
			target := s
			for _, candidate := range m.sessions {
				if candidate.ID == params.SessionID {
					target = candidate
				}
			}
			m.mu.Unlock()
			return onPrompt(target, f)
		}
		if f.Method == "session/new" {
			return map[string]any{"sessionId": newID("native-"), "configOptions": options}
		}
		return map[string]any{"configOptions": options}
	})
	sessionLife, sessionCancel := context.WithCancel(life)
	s = &session{ID: "native", tenant: "tenant", owner: "owner", model: "agy/test", system: digest(""), tools: []tool{}, advertisedTools: []tool{}, responses: map[string]bool{"previous": true}, pending: map[string]*pendingTool{}, reported: map[string]reportedTool{}, events: make(chan agentEvent, 32), life: sessionLife, cancel: sessionCancel, proc: p, options: options, last: time.Now()}
	m = &Manager{cfg: RuntimeConfig{Home: t.TempDir(), IdleTimeout: time.Second, MaxSessions: 3}, life: life, proc: p, sessions: map[string]*session{s.tenant: s}, slot: make(chan struct{}, 3)}
	return m, s
}

func recoveryRequest(m *Manager, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(body))
	r.Header.Set("X-Portal-Owner", "owner")
	m.responsesHTTP(w, r)
	return w
}

func TestAGYLateToolResultStartsFollowupWithActualImageAndSteering(t *testing.T) {
	var calls atomic.Int64
	var prompt string
	m, s := recoveryFixture(t, func(s *session, f frame) any {
		calls.Add(1)
		prompt = string(f.Params)
		s.events <- agentEvent{Text: "recognized actual screenshot"}
		return map[string]any{"stopReason": "end_turn"}
	})
	c := &pendingTool{ID: "call-late", Tool: tool{Type: "function", Name: "screenshot"}, Delivered: true, Relayed: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":[{"type":"function_call_output","call_id":"call-late","output":[{"type":"image","mimeType":"image/png","data":"YQ=="}]},{"role":"user","content":"Only inspect it; do not edit anything."}]}`)
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "recognized actual screenshot") || !strings.Contains(prompt, "Only inspect it") || !strings.Contains(prompt, `\"type\":\"image\"`) && !strings.Contains(prompt, `"type":"image"`) {
		t.Fatalf("late continuation lost content: %d %s %s", w.Code, w.Body.String(), prompt)
	}
	if !c.Replayed {
		t.Fatal("late result was not consumed")
	}
	if blocks, waiting, err := lateResultPrompt(s); err != nil || waiting || len(blocks) != 0 {
		t.Fatal("late result would be replayed twice")
	}
}

func TestAGYLiveToolResultKeepsInterleavedInstructionsAndRejectsInvalidBeforeWake(t *testing.T) {
	a := &pendingTool{ID: "a", Delivered: true, Ready: make(chan struct{})}
	b := &pendingTool{ID: "b", Delivered: true, Ready: make(chan struct{})}
	s := &session{pending: map[string]*pendingTool{"a": a, "b": b}}
	items := []map[string]any{{"type": "function_call_output", "call_id": "a", "output": "ok"}, {"role": "user", "content": "do not modify files"}, {"type": "function_call_output", "call_id": "b", "output": "ok"}, {"role": "developer", "content": "keep this instruction"}}
	if err := deliverToolsForRequest(s, items, &request{}); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(b.Extra)
	if !strings.Contains(string(encoded), "do not modify files") || !strings.Contains(string(encoded), "keep this instruction") {
		t.Fatal("interleaved instructions were dropped")
	}
	c := &pendingTool{ID: "c", Delivered: true, Ready: make(chan struct{})}
	s.pending = map[string]*pendingTool{"c": c}
	bad := []map[string]any{{"type": "function_call_output", "call_id": "c", "output": "ok"}, {"role": "assistant", "content": "fabricated"}}
	if deliverToolsForRequest(s, bad, &request{}) == nil {
		t.Fatal("invalid companion accepted")
	}
	select {
	case <-c.Ready:
		t.Fatal("invalid result woke a tool")
	default:
	}
}

func TestAGYNativeCompletionBeforeClientResultKeepsConversationWaiting(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.running = true
	c := &pendingTool{ID: "call", Delivered: true, Relayed: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	// Model completion was already queued when the client submits its result.
	s.events <- agentEvent{Result: &promptResult{Stop: "end_turn"}}
	var prompts atomic.Int64
	// Replace the fixture's on-prompt handler with a separate process.
	s.proc = diskTestProcess(t, func(f frame) any {
		if f.Method == "session/prompt" {
			prompts.Add(1)
			s.events <- agentEvent{Text: "continued after end"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":[{"type":"function_call_output","call_id":"call","output":"real result"}]}`)
	if w.Code != 200 || prompts.Load() != 1 || !strings.Contains(w.Body.String(), "continued after end") {
		t.Fatalf("early completion lost late result: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYOutputCapContinuesWithoutRepeatingTools(t *testing.T) {
	var calls atomic.Int64
	m, _ := recoveryFixture(t, func(s *session, f frame) any {
		n := calls.Add(1)
		s.events <- agentEvent{Text: "part"}
		stop := "end_turn"
		if n < 3 {
			stop = "max_tokens"
		}
		return map[string]any{"stopReason": stop}
	})
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"finish a long answer"}`)
	if w.Code != 200 || calls.Load() != 3 || !strings.Contains(w.Body.String(), "partpartpart") {
		t.Fatalf("continuation failed %d %s", w.Code, w.Body.String())
	}
	var endless atomic.Int64
	m, _ = recoveryFixture(t, func(s *session, f frame) any { endless.Add(1); return map[string]any{"stopReason": "max_tokens"} })
	w = recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"bounded"}`)
	if endless.Load() != 4 || !strings.Contains(w.Body.String(), "continuation_limit") {
		t.Fatalf("unbounded continuation %d %s", endless.Load(), w.Body.String())
	}
}

func TestAGYHistoryRebuildAndSystemChangeReplayOnlyAuthoritativeContext(t *testing.T) {
	var nativePrompt string
	m, s := recoveryFixture(t, func(s *session, f frame) any {
		nativePrompt = string(f.Params)
		return map[string]any{"stopReason": "end_turn"}
	})
	// Rebuilt sessions route their notifications through the manager normally;
	// this fake transport only needs the completed prompt result.
	s.items = []map[string]any{{"role": "user", "content": "old content"}, {"type": "function_call", "call_id": "old-call", "name": "read", "arguments": "{}"}}
	s.history = requestFingerprints(s.items)
	s.running = true
	s.pending["old-call"] = &pendingTool{ID: "old-call", Delivered: true, Ready: make(chan struct{})}
	w := recoveryRequest(m, `{"model":"agy/test","instructions":"new system instruction","previous_response_id":"previous","input":[{"role":"user","content":"authoritative compressed summary"},{"type":"function_call","call_id":"old-call","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"old-call","output":"completed once"},{"role":"user","content":"continue"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"completed"`) || !strings.Contains(nativePrompt, "authoritative compressed summary") || strings.Contains(nativePrompt, "old content") {
		t.Fatalf("rebuild used stale history: %d %s %s", w.Code, w.Body.String(), nativePrompt)
	}
	for _, next := range m.sessions {
		if len(next.pending) != 0 {
			t.Fatal("history tools were re-executed")
		}
	}
}

func TestAGYIdenticalRequestCanRetryAfterFailure(t *testing.T) {
	var calls atomic.Int64
	m, _ := recoveryFixture(t, func(s *session, f frame) any {
		if calls.Add(1) == 1 {
			return map[string]any{"stopReason": "cancelled"}
		}
		s.events <- agentEvent{Text: "retry completed"}
		return map[string]any{"stopReason": "end_turn"}
	})
	const body = `{"model":"agy/test","input":"retry the identical request"}`
	first := recoveryRequest(m, body)
	if !strings.Contains(first.Body.String(), "agent_incomplete") {
		t.Fatalf("first request should fail: %d %s", first.Code, first.Body.String())
	}
	second := recoveryRequest(m, body)
	if second.Code != 200 || calls.Load() != 2 || !strings.Contains(second.Body.String(), "retry completed") || !strings.Contains(second.Body.String(), `"status":"completed"`) {
		t.Fatalf("identical retry blocked: %d %s calls=%d", second.Code, second.Body.String(), calls.Load())
	}
}

func TestAGYSameSessionConcurrentRequestRemainsBlocked(t *testing.T) {
	var calls atomic.Int64
	m, s := recoveryFixture(t, func(*session, frame) any {
		calls.Add(1)
		return map[string]any{"stopReason": "end_turn"}
	})
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"continue"}`)
	if !strings.Contains(w.Body.String(), "session_busy") || calls.Load() != 0 {
		t.Fatalf("concurrent request reached active session: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYToolIDRenameRequiresExactFullHistory(t *testing.T) {
	s := &session{items: []map[string]any{{"role": "user", "content": "read"}, {"type": "function_call", "call_id": "original-a", "name": "read", "arguments": "{\"path\":\"a\"}"}, {"type": "function_call", "call_id": "original-b", "name": "read", "arguments": "{\"path\":\"b\"}"}}}
	s.history = requestFingerprints(s.items)
	items := copyItems(s.items)
	items[2]["call_id"] = "renamed-b"
	items = append(items, map[string]any{"type": "function_call_output", "call_id": "renamed-b", "output": "done"})
	next, ok := alignToolIDs(s, items)
	if !ok || next[3]["call_id"] != "original-b" || items[3]["call_id"] != "renamed-b" {
		t.Fatal("partial ID rename corrupted mapping")
	}
	items[2]["arguments"] = "{\"path\":\"other\"}"
	if _, ok = alignToolIDs(s, items); ok {
		t.Fatal("changed tool arguments established an alias")
	}
	if _, ok = alignToolIDs(s, items[3:]); ok {
		t.Fatal("result alone established an alias")
	}
}

func TestAGYMCPIndividualCancelPreservesTypedIDsAndOtherCalls(t *testing.T) {
	s := &session{}
	numeric, cleanup, ok := registerMCP(s, "1")
	if !ok {
		t.Fatal("numeric registration")
	}
	defer cleanup()
	text, done, ok := registerMCP(s, `"1"`)
	if !ok {
		t.Fatal("string registration")
	}
	defer done()
	m := &Manager{}
	m.cancelMCP(s, json.RawMessage(`{"requestId":1}`))
	select {
	case <-numeric:
	default:
		t.Fatal("numeric cancellation missed")
	}
	select {
	case <-text:
		t.Fatal("cancelled wrong request")
	default:
	}
	if _, _, ok = registerMCP(s, `"1"`); ok {
		t.Fatal("duplicate active ID accepted")
	}
}

func TestAGYConcurrentChatsDeliverResultsIndependently(t *testing.T) {
	m, base := recoveryFixture(t, func(*session, frame) any { return map[string]any{"stopReason": "end_turn"} })
	m.sessions = map[string]*session{}
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	var workers sync.WaitGroup
	for _, id := range []string{"a", "b", "c"} {
		life, cancel := context.WithCancel(m.life)
		defer cancel()
		call := &pendingTool{ID: "call-" + id, Delivered: true, Ready: make(chan struct{})}
		s := &session{ID: id, tenant: id, owner: "owner", model: "agy/test", system: digest(""), responses: map[string]bool{"previous-" + id: true}, pending: map[string]*pendingTool{call.ID: call}, events: make(chan agentEvent, 4), life: life, cancel: cancel, proc: base.proc, running: true}
		m.sessions[id] = s
		go func() {
			<-call.Ready
			s.mu.Lock()
			call.Returned = true
			s.mu.Unlock()
			started <- struct{}{}
			<-release
			s.events <- agentEvent{Text: "independent"}
			s.events <- agentEvent{Result: &promptResult{Stop: "end_turn"}}
		}()
	}
	for _, id := range []string{"a", "b", "c"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous-`+id+`","input":[{"type":"function_call_output","call_id":"call-`+id+`","output":"done"}]}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "independent") {
				t.Errorf("chat %s blocked: %d %s", id, w.Code, w.Body.String())
			}
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(750 * time.Millisecond):
			close(release)
			workers.Wait()
			t.Fatal("independent chats serialized behind a global slot")
		}
	}
	close(release)
	workers.Wait()
}

func TestAGYRebuildInvalidatesOldDiskRoutingAndKeepsEncryptedHistory(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return map[string]any{"stopReason": "end_turn"} })
	d, err := openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	s.items = []map[string]any{{"role": "user", "content": "private original text"}}
	s.history = requestFingerprints(s.items)
	if err = m.saveSession(s, true); err != nil {
		t.Fatal(err)
	}
	r := request{Model: s.model, Previous: "previous", Items: []map[string]any{{"role": "user", "content": "updated context"}}}
	r.Instructions = json.RawMessage(`"updated system"`)
	next, err := m.rebuildSession(t.Context(), s, r)
	if err != nil {
		t.Fatal(err)
	}
	var ready int
	if err = d.db.QueryRow(`SELECT ready FROM sessions WHERE id=?`, s.ID).Scan(&ready); err != nil || ready != 0 {
		t.Fatal("old response routing remained restorable")
	}
	if next.ID == s.ID || !next.responses["previous"] {
		t.Fatal("new routing lost the explicit response chain")
	}
	var payload []byte
	if err = d.db.QueryRow(`SELECT payload FROM sessions WHERE id=?`, s.ID).Scan(&payload); err != nil || strings.Contains(string(payload), "private original text") {
		t.Fatal("wire history was stored without encryption")
	}
}
