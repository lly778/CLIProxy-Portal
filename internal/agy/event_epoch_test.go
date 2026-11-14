package agy

import (
	"strings"
	"testing"
)

func TestRestoreEchoesWhileLoadingAndIdleAreNotNewOutput(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	for _, loading := range []bool{true, false} {
		s.loading, s.running = loading, !loading
		if !loading {
			s.running = false
		}
		retryTestText(m, s, "old restore text", false)
		retryTestUpdate(m, s, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "old-google", "name": "search_web", "status": "completed"})
		if len(s.events) != 0 || len(s.reported) != 0 {
			t.Fatal("restore echo became new output/action")
		}
	}
	s.running, s.epoch = true, 2
	retryTestText(m, s, "real text", false)
	if event := <-s.events; event.Text != "real text" || event.Epoch != 2 {
		t.Fatal("real output lost identity", event)
	}
}

func TestStaleQueuedCompletionAndToolEventsCannotEndNewTurn(t *testing.T) {
	var m *Manager
	m, s := recoveryFixture(t, func(s *session, frame frame) any {
		retryTestText(m, s, "real new reply", false)
		return map[string]any{"stopReason": "end_turn"}
	})
	s.epoch = 1
	s.events <- agentEvent{Epoch: 1, Text: "old queued echo"}
	s.events <- agentEvent{Epoch: 1, Official: &officialToolEvent{ID: "old", Name: "search_web", Status: "completed"}}
	s.events <- agentEvent{Epoch: 1, Result: &promptResult{Stop: "end_turn"}}
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"next"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "real new reply") || strings.Contains(w.Body.String(), "old queued echo") || strings.Contains(w.Body.String(), "web_search_call") {
		t.Fatalf("old epoch leaked or ended new turn: %d %.1200s", w.Code, w.Body.String())
	}
}
