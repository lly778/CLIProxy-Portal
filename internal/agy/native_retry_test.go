package agy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func retryTestUpdate(m *Manager, s *session, update map[string]any) {
	b, _ := json.Marshal(map[string]any{"sessionId": s.ID, "update": update})
	m.update(b)
}

func retryTestText(m *Manager, s *session, text string, thought bool) {
	kind := "agent_message_chunk"
	if thought {
		kind = "agent_thought_chunk"
	}
	retryTestUpdate(m, s, map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}})
}

const retryTestWarning = "⚠ API error: UNAVAILABLE: No capacity available for model test on the server"

func TestAGYWarningWaitsForNativeRetryAndRecoveredAnswer(t *testing.T) {
	warned, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	var calls atomic.Int64
	var m *Manager
	var s *session
	m, s = recoveryFixture(t, func(target *session, f frame) any {
		calls.Add(1)
		retryTestText(m, target, retryTestWarning, false)
		close(warned)
		<-resume // The native RPC stays in flight while the CLI backs off.
		retryTestText(m, target, "recovered answer", false)
		return map[string]any{"stopReason": "end_turn"}
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"continue","stream":true}`)
	}()
	select {
	case <-warned:
	case <-time.After(time.Second):
		t.Fatal("native prompt was not started")
	}
	select {
	case <-s.life.Done():
		t.Fatal("intermediate API warning cancelled the native retry")
	case <-done:
		t.Fatal("request ended before the native RPC finished")
	case <-time.After(25 * time.Millisecond):
	}
	release.Do(func() { close(resume) })
	select {
	case w := <-done:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "recovered answer") || !strings.Contains(w.Body.String(), `"status":"completed"`) || strings.Contains(w.Body.String(), "response.failed") || strings.Contains(w.Body.String(), "No capacity") || calls.Load() != 1 {
			t.Fatalf("native recovery failed or prompt was resubmitted: calls=%d %d %s", calls.Load(), w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("native recovery did not finish")
	}
}

func TestAGYFinalNativeWarningRemainsFailureAfterPartialOutput(t *testing.T) {
	for _, partial := range []bool{false, true} {
		var m *Manager
		m, _ = recoveryFixture(t, func(s *session, f frame) any {
			if partial {
				retryTestText(m, s, "partial answer", false)
			}
			retryTestText(m, s, retryTestWarning, false)
			retryTestText(m, s, "still thinking", true)
			return map[string]any{"stopReason": "end_turn"}
		})
		w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"continue","stream":true}`)
		if !strings.Contains(w.Body.String(), "response.failed") || !strings.Contains(w.Body.String(), "upstream_capacity_exhausted") || strings.Contains(w.Body.String(), `"status":"completed"`) {
			t.Fatalf("partial=%v: final warning became success: %s", partial, w.Body.String())
		}
	}
}

func TestAGYWarningRecoveryRecognizesNewToolsButNotStaleUpdates(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	for _, tc := range []struct {
		kind, status string
		alreadySeen  bool
		recovered    bool
	}{
		{"tool_call", "in_progress", false, true},
		{"tool_call", "failed", false, false},
		{"tool_call_update", "completed", true, false},
		{"tool_call", "in_progress", true, false},
	} {
		s.mu.Lock()
		s.reported = map[string]reportedTool{}
		if tc.alreadySeen {
			s.reported["call"] = reportedTool{Server: bridgeName, Name: callToolName}
		}
		s.mu.Unlock()
		retryTestText(m, s, retryTestWarning, false)
		retryTestUpdate(m, s, map[string]any{"sessionUpdate": tc.kind, "toolCallId": "call", "mcpServerName": bridgeName, "mcpToolName": callToolName, "status": tc.status})
		if err := s.finishPrompt(nil); (err == nil) != tc.recovered {
			t.Fatalf("%+v: recovery result %v", tc, err)
		}
	}
}

func TestAGYFinalWarningPreservesCancellationAndSpecificRPCFailures(t *testing.T) {
	for _, original := range []error{nil, fail(502, "agent_error", "generic RPC failure"), context.Canceled, fail(503, "worker_lost", "transport closed"), fail(502, "upstream_access_denied", "access denied")} {
		warning := upstreamFailure("No capacity available")
		s := &session{promptWarning: warning}
		got := s.finishPrompt(original)
		if original == nil || original.Error() == "generic RPC failure" {
			if got != warning {
				t.Fatalf("specific warning was lost: %v", got)
			}
		} else if !errors.Is(got, original) {
			t.Fatalf("cancellation/transport/terminal error replaced: %v", got)
		}
		if s.finishPrompt(nil) != nil {
			t.Fatal("warning leaked into a later prompt")
		}
	}
}

func TestAGYSummaryWaitsForNativeRecoveryAndRejectsFinalWarnings(t *testing.T) {
	for _, recover := range []bool{false, true} {
		var m *Manager
		m, _ = recoveryFixture(t, func(s *session, f frame) any {
			retryTestText(m, s, retryTestWarning, false)
			if recover {
				retryTestText(m, s, "authoritative summary", false)
			}
			return map[string]any{"stopReason": "end_turn"}
		})
		text, err := m.summaryTurn(t.Context(), "agy/test", []any{map[string]string{"type": "text", "text": "summarize history"}}, nil)
		if recover {
			if err != nil || text != "authoritative summary" {
				t.Fatalf("recovered summary was rejected: %s %v", text, err)
			}
		} else if err == nil || text != "" {
			t.Fatalf("failed summary was accepted: %s %v", text, err)
		}
	}
}
