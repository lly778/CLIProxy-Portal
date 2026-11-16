package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
const retryTestTermination = "\n⚠ Agent execution terminated due to error.\n"

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
	s.running = true
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
	for _, original := range []error{nil, fail(502, "agent_error", "generic RPC failure"), fail(502, "agent_execution_failed", "generic RPC failure"), context.Canceled, fail(503, "worker_lost", "transport closed"), fail(502, "upstream_access_denied", "access denied")} {
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

func TestAGYExecutionTerminationIsFailureAndPreservesRetryReason(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, prior := range []string{"", "⚠ API error (attempt 8): RESOURCE_EXHAUSTED (code 429): Resource has been exhausted (e.g. check quota).", retryTestWarning} {
			t.Run(fmt.Sprintf("stream=%v/prior=%s", stream, prior), func(t *testing.T) {
				var calls atomic.Int64
				var m *Manager
				m, _ = recoveryFixture(t, func(s *session, f frame) any {
					calls.Add(1)
					if prior != "" {
						for i := 0; i < 8; i++ {
							retryTestText(m, s, prior, false)
						}
					}
					retryTestText(m, s, retryTestTermination, false)
					return map[string]any{"stopReason": "end_turn"}
				})
				body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "input": "continue", "stream": stream})
				w := recoveryRequest(m, string(body))
				tracker := &Tracker{Stream: stream}
				tracker.Feed(w.Body.Bytes())
				tracker.Finish()
				want := "agent_execution_failed"
				if prior != "" {
					want = agentFailure(prior).(*Error).Code
				}
				if tracker.Completed || tracker.Failure != want || tracker.Usage.Known || calls.Load() != 1 || strings.Contains(w.Body.String(), `"status":"completed"`) || strings.Contains(w.Body.String(), "⚠") || strings.Contains(w.Body.String(), `"role":"assistant"`) {
					t.Fatalf("native termination became success or lost its reason: %d %s", w.Code, w.Body.String())
				}
				if !stream && w.Code != FailureStatus(want) {
					t.Fatalf("status = %d, want %d", w.Code, FailureStatus(want))
				}
			})
		}
	}
}

func TestAGYExecutionTerminationDoesNotLeakIntoNextPrompt(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.running = true
	retryTestText(m, s, retryTestTermination, false)
	if err := s.finishPrompt(nil); err == nil || err.(*Error).Code != "agent_execution_failed" {
		t.Fatal("terminal marker was not recognized", err)
	}
	if s.finishPrompt(nil) != nil {
		t.Fatal("terminal marker leaked into a later prompt")
	}
	for _, ordinary := range []string{
		"The log contains: ⚠ Agent execution terminated due to error.",
		"`⚠ Agent execution terminated due to error.` is a diagnostic example.",
		"Agent execution terminated due to error.",
		"⚠ Agent execution terminated due to error is only an example in the document.",
	} {
		if agentFailure(ordinary) != nil {
			t.Fatal("ordinary assistant text became a provider failure", ordinary)
		}
	}
}

func TestAGYSummaryRejectsExecutionTerminationMarker(t *testing.T) {
	var m *Manager
	m, _ = recoveryFixture(t, func(s *session, f frame) any {
		retryTestText(m, s, retryTestTermination, false)
		return map[string]any{"stopReason": "end_turn"}
	})
	text, err := m.summaryTurn(t.Context(), "agy/test", []any{map[string]string{"type": "text", "text": "summarize history"}}, nil)
	if err == nil || text != "" {
		t.Fatal("termination notice was accepted as a successful summary", text, err)
	}
}

func TestAGYGenericTerminationNoticeDoesNotCancelNativeRecovery(t *testing.T) {
	var m *Manager
	var calls atomic.Int64
	m, _ = recoveryFixture(t, func(s *session, f frame) any {
		calls.Add(1)
		retryTestText(m, s, retryTestTermination, false)
		retryTestText(m, s, "actual answer after native recovery", false)
		return map[string]any{"stopReason": "end_turn"}
	})
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"continue","stream":true}`)
	tracker := &Tracker{Stream: true}
	tracker.Feed(w.Body.Bytes())
	tracker.Finish()
	if !tracker.Completed || tracker.Failure != "" || calls.Load() != 1 || !strings.Contains(w.Body.String(), "actual answer after native recovery") || strings.Contains(w.Body.String(), "terminated due to error") {
		t.Fatal("recovered native response was cancelled or diagnostic was exposed", w.Body.String())
	}
}
