package agy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAGYInterruptedGenerationPreservesRetryChainAndPartialOutput(t *testing.T) {
	var calls atomic.Int64
	var resumedPrompt string
	m, _ := recoveryFixture(t, func(s *session, f frame) any {
		if calls.Add(1) == 1 {
			s.events <- agentEvent{Text: "analysis already written before interruption"}
			return map[string]any{"stopReason": "cancelled"}
		}
		resumedPrompt = string(f.Params)
		s.events <- agentEvent{Text: "resumed"}
		return map[string]any{"stopReason": "end_turn"}
	})
	d, err := openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	body := `{"model":"agy/test","previous_response_id":"previous","input":"continue this task"}`
	first := recoveryRequest(m, body)
	if !strings.Contains(first.Body.String(), "agent_incomplete") || len(m.sessions) != 0 {
		t.Fatal("fixture generation did not fail and close", first.Body.String())
	}
	cold, err := m.find("owner", request{Previous: "previous"})
	if err != nil || cold == nil || !cold.interrupted || len(cold.interruptedOutput) == 0 {
		t.Fatal("failed continuation discarded its recovery state", err)
	}
	second := recoveryRequest(m, body)
	if second.Code != 200 || calls.Load() != 2 || !strings.Contains(second.Body.String(), "resumed") || !strings.Contains(resumedPrompt, "analysis already written before interruption") || !strings.Contains(resumedPrompt, "not a completed answer") {
		t.Fatalf("failed generation was blocked, treated as completed, or lost progress: %d %d %s %s", second.Code, calls.Load(), second.Body.String(), resumedPrompt)
	}
}

func TestAGYWaitingCheckpointIsEncryptedWhenRemovedFromMemory(t *testing.T) {
	m, s, _, _ := waitingCacheFixture(t)
	if err := m.archiveWaitingSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	cold, err := m.disk.find("owner", request{Previous: "previous"}, nil)
	if err != nil || cold == nil || cold.pending["call-once"] == nil || cold.interrupted {
		t.Fatal("removed tool wait lost its recovery state", err)
	}
	var payload []byte
	if err := m.disk.db.QueryRow(`SELECT payload FROM sessions WHERE id=?`, s.ID).Scan(&payload); err != nil || strings.Contains(string(payload), "write once") {
		t.Fatal("waiting checkpoint was not encrypted", err)
	}
}

func TestAGYGracefulClosePreservesWaitingAndCompletedSessions(t *testing.T) {
	m, s, _, _ := waitingCacheFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	m.life, m.cancel = ctx, cancel
	m.mcpServer = &http.Server{}
	if !m.beginRequest() {
		t.Fatal("could not register in-flight request")
	}
	go func() {
		<-ctx.Done()
		_ = m.preserveSession(s, nil)
		m.requests.Done()
	}()
	m.Close()
	if m.beginRequest() {
		t.Fatal("shutdown accepted a new request")
	}
	d, err := openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	cold, err := d.find("owner", request{Previous: "previous"}, nil)
	if err != nil || cold == nil || cold.pending["call-once"] == nil {
		t.Fatal("shutdown closed the database before checkpoint completion", err)
	}
}

func TestAGYResponseHistoryCountDoesNotCancelLongConversation(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	for i := 0; i < 300; i++ {
		m.remember(s, &response{id: newID("resp_")})
	}
	if s.life.Err() != nil || len(s.responses) != 301 {
		t.Fatal("response count closed a healthy conversation")
	}
}

func TestAGYInterruptedCheckpointNeverInventsCompletionOrToolResults(t *testing.T) {
	m, s, _, _ := waitingCacheFixture(t)
	c := s.pending["call-once"]
	s.items = s.items[:1]
	s.history = requestFingerprints(s.items)
	s.waitAt = time.Time{}
	out := &response{output: []map[string]any{{"type": "message", "content": []any{}}}, textValue: "unfinished answer"}
	if err := m.preserveSession(s, out); err != nil {
		t.Fatal(err)
	}
	cold, err := m.disk.find("owner", request{Previous: "previous"}, nil)
	if err != nil || cold == nil || cold.pending[c.ID] == nil || len(cold.items) != 2 {
		t.Fatal("partially delivered client call was lost", err)
	}
	select {
	case <-cold.pending[c.ID].Ready:
		t.Fatal("missing tool result was invented")
	default:
	}
	b, _ := json.Marshal(cold.interruptedOutput)
	if !strings.Contains(string(b), "unfinished answer") || !strings.Contains(string(b), "incomplete") {
		t.Fatal("partial answer was discarded or marked completed")
	}
}

func TestAGYExpiredWaitWithConsumedResultsPreservesInterruptedState(t *testing.T) {
	m, s, _, _ := waitingCacheFixture(t)
	s.pending["call-once"].Returned = true
	m.sweepSession(t.Context(), s, time.Now())
	if len(m.sessions) != 0 || s.life.Err() == nil {
		t.Fatal("expired native continuation kept an occupied worker forever")
	}
	cold, err := m.disk.find("owner", request{Previous: "previous"}, nil)
	if err != nil || cold == nil || !cold.interrupted || len(cold.pending) != 0 {
		t.Fatal("orphaned native continuation was lost or restored as a live waiter", err)
	}
}
