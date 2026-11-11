package agy

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAGYGenerationsAndToolHandoffsSaveSessionOnlyOnRemoval(t *testing.T) {
	var calls atomic.Int64
	m, s := recoveryFixture(t, func(s *session, f frame) any {
		if calls.Add(1) == 2 {
			c := &pendingTool{ID: "call-saved-on-removal", Tool: tool{Type: "function", Name: "write"}, Arguments: map[string]any{}, Ready: make(chan struct{})}
			s.mu.Lock()
			s.pending[c.ID] = c
			s.mu.Unlock()
			s.events <- agentEvent{Call: c}
		} else {
			s.events <- agentEvent{Text: "completed response"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	d, err := openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	previous := "previous"
	for _, input := range []string{`"first"`, `"write once"`, `[{"type":"function_call_output","call_id":"call-saved-on-removal","output":"written once"}]`} {
		w := recoveryRequest(m, fmt.Sprintf(`{"model":"agy/test","previous_response_id":%q,"input":%s}`, previous, input))
		var response struct{ ID string }
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.ID == "" {
			t.Fatalf("fixture request failed: %d %s %v", w.Code, w.Body.String(), err)
		}
		previous = response.ID
		var count int
		if err := d.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("normal generation/tool handoff wrote a session checkpoint: %d %v", count, err)
		}
	}
	if err := m.archiveSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	delete(m.sessions, s.tenant)
	cold, err := d.find("owner", request{Previous: previous}, nil)
	if err != nil || cold == nil || cold.ID != s.ID {
		t.Fatal("removal did not save the latest response chain", err)
	}
	// Once loaded back into memory, the old disk checkpoint must not be
	// accepted after a crash against a native session that may have advanced.
	if _, err := m.restoreSession(t.Context(), cold, request{Model: "agy/test"}); err != nil {
		t.Fatal(err)
	}
	if old, err := d.find("owner", request{Previous: previous}, nil); err != nil || old != nil {
		t.Fatal("resident session kept a stale disk recovery point", err)
	}
}
