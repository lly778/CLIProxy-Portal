package agy

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitingCacheFixture(t *testing.T) (*Manager, *session, *atomic.Int64, *string) {
	t.Helper()
	var prompts atomic.Int64
	prompt := ""
	m, s := recoveryFixture(t, func(target *session, f frame) any {
		prompts.Add(1)
		prompt = string(f.Params)
		target.events <- agentEvent{Text: "continued with the original tool result"}
		return map[string]any{"stopReason": "end_turn"}
	})
	disk, err := openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.db.Close() })
	m.disk, m.cfg.ToolTimeout = disk, 10*time.Minute
	s.officialVersion = officialToolsVersion
	s.items = []map[string]any{{"role": "user", "content": "write once"}, {"type": "function_call", "name": "write", "call_id": "call-once", "arguments": `{}`}}
	s.history = requestFingerprints(s.items)
	s.pending["call-once"] = &pendingTool{ID: "call-once", Tool: tool{Type: "function", Name: "write"}, Arguments: map[string]any{}, Delivered: true, Relayed: true, Ready: make(chan struct{})}
	s.running, s.waitAt, s.last = true, time.Now().Add(-11*time.Minute), time.Now().Add(-11*time.Minute)
	return m, s, &prompts, &prompt
}

func TestAGYToolWaitTimeoutArchivesAndRestoresOriginalCall(t *testing.T) {
	m, original, calls, prompt := waitingCacheFixture(t)
	m.sweepSession(t.Context(), original, time.Now())
	if len(m.sessions) != 0 || original.life.Err() == nil {
		t.Fatal("expired waiting worker was not released")
	}
	cold, err := m.find("owner", request{Previous: "previous"})
	if err != nil || cold == nil || cold.ID != original.ID || cold.running || cold.pending["call-once"] == nil {
		t.Fatalf("original response/call identity was lost: %+v %v", cold, err)
	}
	if other, err := m.find("other:key", request{Previous: "previous"}); err == nil || other != nil {
		t.Fatal("suspended tool result crossed the owner/key boundary")
	}
	// The encrypted record is durable across a manager restart, not merely an
	// in-memory copy left by archiveWaitingSession.
	disk := m.disk
	if err := disk.db.Close(); err != nil {
		t.Fatal(err)
	}
	m.disk, err = openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.disk.db.Close() })
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":[{"type":"function_call_output","call_id":"call-once","output":"written exactly once"}]}`)
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "continued with the original tool result") || strings.Contains(w.Body.String(), `"type":"function_call"`) {
		t.Fatalf("restore reissued the client action: calls=%d %d %s", calls.Load(), w.Code, w.Body.String())
	}
	if !strings.Contains(*prompt, "written exactly once") || !strings.Contains(*prompt, "do not execute again") {
		t.Fatalf("restore did not pass the actual result as historical context: %s", *prompt)
	}
	for _, s := range m.sessions {
		if s.ID != original.ID {
			t.Fatal("native session was replaced instead of loaded")
		}
	}
}

func TestAGYToolWaitSaveFailureKeepsOriginalWorker(t *testing.T) {
	m, s, _, _ := waitingCacheFixture(t)
	if err := m.disk.db.Close(); err != nil {
		t.Fatal(err)
	}
	m.sweepSession(t.Context(), s, time.Now())
	if m.sessions[s.tenant] != s || s.life.Err() != nil || !s.running || !m.cacheError {
		t.Fatal("failed checkpoint discarded the live waiting conversation")
	}
}

func TestAGYToolWaitKeepsPartialBatchAndImagesOnDisk(t *testing.T) {
	m, s, _, _ := waitingCacheFixture(t)
	c := s.pending["call-once"]
	c.Output = []any{map[string]any{"type": "image", "mimeType": "image/png", "data": "YQ=="}}
	c.OutputHash = digest(c.Output)
	c.Extra = []any{map[string]string{"type": "text", "text": "client steering"}}
	close(c.Ready)
	s.pending["call-other"] = &pendingTool{ID: "call-other", Tool: tool{Type: "function", Name: "read"}, Delivered: true, Ready: make(chan struct{})}
	s.items = append(s.items, map[string]any{"type": "function_call", "name": "read", "call_id": "call-other", "arguments": `{}`})
	s.history = requestFingerprints(s.items)
	m.sweepSession(t.Context(), s, time.Now())
	cold, err := m.find("owner", request{Previous: "previous"})
	if err != nil || cold == nil {
		t.Fatal("suspended batch was lost", err)
	}
	if blocks, waiting, err := lateResultPrompt(cold); err != nil || !waiting || blocks != nil {
		t.Fatal("partial batch was resumed without all client results", err)
	}
	if err := deliverTools(cold, []map[string]any{{"type": "function_call_output", "call_id": "call-other", "output": "actual read result"}}); err != nil {
		t.Fatal(err)
	}
	blocks, waiting, err := lateResultPrompt(cold)
	b, _ := json.Marshal(blocks)
	if err != nil || waiting || !strings.Contains(string(b), `"type":"image"`) || !strings.Contains(string(b), "client steering") || !strings.Contains(string(b), "actual read result") {
		t.Fatalf("saved image/partial batch was not preserved: %s %v", b, err)
	}
}

func TestAGYToolWaitDoesNotArchiveBeforeItsTimeoutOrDuringRequest(t *testing.T) {
	m, s, _, _ := waitingCacheFixture(t)
	s.waitAt = time.Now().Add(-9 * time.Minute)
	m.sweepSession(t.Context(), s, time.Now())
	if m.sessions[s.tenant] != s || s.life.Err() != nil {
		t.Fatal("tool wait was archived before its ten-minute timeout")
	}
	s.waitAt = time.Now().Add(-11 * time.Minute)
	s.requestMu.Lock()
	m.sweepSession(t.Context(), s, time.Now())
	s.requestMu.Unlock()
	if m.sessions[s.tenant] != s || s.life.Err() != nil {
		t.Fatal("active result delivery raced a suspension")
	}
}
