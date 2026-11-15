package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func identifiedRecoveryRequest(m *Manager, body, identity string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(body))
	r.Header.Set("X-Portal-Owner", "owner")
	r.Header.Set("Idempotency-Key", identity)
	w := httptest.NewRecorder()
	m.responsesHTTP(w, r)
	return w
}

func TestLostResponseCacheResumesOriginalTaskWithoutRedispatch(t *testing.T) {
	var prompts atomic.Int64
	m, source := recoveryFixture(t, func(target *session, _ frame) any {
		if prompts.Add(1) == 1 {
			c := &pendingTool{ID: "original-write", Tool: tool{Type: "function", Name: "write"}, Arguments: map[string]any{}, Ready: make(chan struct{})}
			target.mu.Lock()
			target.pending[c.ID] = c
			target.mu.Unlock()
			target.events <- agentEvent{Call: c}
		} else {
			target.events <- agentEvent{Text: "continued with original actual result"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	d := protocolDisk(t, m)
	source.officialVersion = officialToolsVersion
	body := `{"model":"agy/test","previous_response_id":"previous","input":"write once"}`
	first := identifiedRecoveryRequest(m, body, "original-request")
	var result struct{ ID string }
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil || first.Code != 200 || result.ID == "" {
		t.Fatal("initial handoff failed", first.Body.String(), err)
	}
	// Completed response reuse still has priority while its cache survives.
	cached := identifiedRecoveryRequest(m, body, "original-request")
	if cached.Code != 200 || cached.Body.String() != first.Body.String() || prompts.Load() != 1 {
		t.Fatal("ordinary cached retry changed the tool identity")
	}
	if err := m.clearResponseReplays(); err != nil {
		t.Fatal(err)
	}
	// Retain the original save-on-removal policy: cold protection is available
	// after an actual archive, not by adding writes to normal tool handoffs.
	if err := m.archiveWaitingSession(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	retry := identifiedRecoveryRequest(m, body, "original-request")
	if retry.Code != 200 || strings.Contains(retry.Body.String(), `"type":"function_call"`) || prompts.Load() != 2 {
		t.Fatal("lost replay cache failed to resume without redispatch", prompts.Load(), retry.Body.String())
	}
	var recovered struct{ ID string }
	if err := json.Unmarshal(retry.Body.Bytes(), &recovered); err != nil || recovered.ID == "" {
		t.Fatal("recovery did not produce a continuation identity", err)
	}
	replayed := identifiedRecoveryRequest(m, body, "original-request")
	if replayed.Body.String() != retry.Body.String() || prompts.Load() != 2 {
		t.Fatal("recovered response was not reused on another identical retry")
	}
	var count int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM session_handoff_routes`).Scan(&count); err != nil || count != 1 {
		t.Fatal("tool handoff identity was not durable", count, err)
	}
	// A new instruction continues this same recovered native conversation.
	nextBody, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": recovered.ID, "input": "continue independent work"})
	next := identifiedRecoveryRequest(m, string(nextBody), "result-request")
	if next.Code != 200 || prompts.Load() != 3 || !strings.Contains(next.Body.String(), "original actual result") {
		t.Fatal("guard blocked original result continuation", prompts.Load(), next.Body.String())
	}
	var resident *session
	for _, candidate := range m.sessions {
		if candidate.ID == source.ID {
			resident = candidate
		}
	}
	if resident == nil {
		t.Fatal("original session was not resident after continuation")
	}
	if err := m.archiveSession(t.Context(), resident); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM session_handoff_routes`).Scan(&count); err != nil || count != 0 {
		t.Fatal("finished handoff left a permanent retry blocker", count, err)
	}
}

func TestFailedGenerationWithoutHandoffStillRetriesNormally(t *testing.T) {
	var prompts atomic.Int64
	var resumed string
	m, _ := recoveryFixture(t, func(target *session, f frame) any {
		if prompts.Add(1) == 1 {
			return map[string]any{"stopReason": "cancelled"}
		}
		resumed = string(f.Params)
		target.events <- agentEvent{Text: "successful ordinary retry"}
		return map[string]any{"stopReason": "end_turn"}
	})
	protocolDisk(t, m)
	body := `{"model":"agy/test","previous_response_id":"previous","input":"unique-user-input-8942"}`
	first := identifiedRecoveryRequest(m, body, "failed-request")
	if !strings.Contains(first.Body.String(), "agent_incomplete") {
		t.Fatal("fixture did not fail generation", first.Body.String())
	}
	next := identifiedRecoveryRequest(m, body, "failed-request")
	if next.Code != 200 || prompts.Load() != 2 || !strings.Contains(next.Body.String(), "successful ordinary retry") {
		t.Fatal("ordinary failure became an unknown-operation tombstone", prompts.Load(), next.Body.String())
	}
	if strings.Count(resumed, "unique-user-input-8942") != 1 {
		t.Fatal("interrupted retry appended the accepted user input twice", resumed)
	}
}

func TestHandoffRetryLookupRemainsOwnerAndPayloadScoped(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	d := protocolDisk(t, m)
	c := continuityCall(s)
	r := replayTestRequest(t.Context(), `{"model":"agy/test","input":"write"}`, "stable")
	key := m.handoffRequestKey("owner", r.Header, []byte(`{"model":"agy/test","input":"write"}`))
	c.requestKey = key
	if err := d.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	if found, err := m.handoffRetrySession("owner", key); err != nil || found == nil || found.ID != s.ID {
		t.Fatal("original unacknowledged request was not recognized", err)
	}
	if found, err := m.handoffRetrySession("other-owner", key); err != nil || found != nil {
		t.Fatal("another owner inherited an old operation", err)
	}
	changed := m.handoffRequestKey("owner", r.Header, []byte(`{"model":"agy/test","input":"verify the effect read-only"}`))
	if found, err := m.handoffRetrySession("owner", changed); err != nil || found != nil {
		t.Fatal("new independent input was treated as a duplicate execution", err)
	}
}
