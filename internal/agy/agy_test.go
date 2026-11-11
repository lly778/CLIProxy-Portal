package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAGYCompletedSessionCacheCanYieldToNewChat(t *testing.T) {
	old := &session{ID: "old-native", tenant: "old", last: time.Now().Add(-time.Minute)}
	m := &Manager{cfg: RuntimeConfig{MaxSessions: 1}, sessions: map[string]*session{"old": old}}
	next := &session{tenant: "new"}
	victim, ok := m.reserveSession(next)
	if !ok || victim != old || len(m.sessions) != 1 || m.sessions["new"] != next {
		t.Fatal("completed session blocked a new conversation")
	}
	victim.requestMu.Unlock()
	for _, kind := range []string{"generating", "tools", "delivering"} {
		t.Run(kind, func(t *testing.T) {
			busy := &session{ID: "busy-native", tenant: "busy"}
			switch kind {
			case "generating":
				busy.running = true
			case "tools":
				busy.waitAt = time.Now()
			case "delivering":
				busy.requestMu.Lock()
				defer busy.requestMu.Unlock()
			}
			m := &Manager{cfg: RuntimeConfig{MaxSessions: 1}, sessions: map[string]*session{"busy": busy}}
			if victim, ok := m.reserveSession(next); ok || victim != nil || m.sessions["busy"] != busy {
				t.Fatal("active session was evicted")
			}
		})
	}
}

func TestAGYStreamToolArgumentsAreNotDuplicated(t *testing.T) {
	w := httptest.NewRecorder()
	r := newResponse(w, true, "agy/test")
	call := &pendingTool{ID: "call_probe", Tool: tool{Type: "function", Name: "probe", Namespace: "functions"}, Arguments: map[string]any{"label": "one"}}
	if e := r.call(call); e != nil {
		t.Fatal(e)
	}
	if e := r.finish(Usage{}); e != nil {
		t.Fatal(e)
	}
	arguments := ""
	created := false
	completed := false
	sequence := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatal(err)
		}
		if e["sequence_number"] != float64(sequence) {
			t.Fatal("non-monotonic event sequence")
		}
		sequence++
		switch e["type"] {
		case "response.created":
			created = true
		case "response.output_item.added":
			item := e["item"].(map[string]any)
			if item["namespace"] != "functions" {
				t.Fatal("tool namespace lost")
			}
			arguments, _ = item["arguments"].(string)
		case "response.function_call_arguments.delta":
			arguments += e["delta"].(string)
		case "response.completed":
			completed = true
		}
	}
	if arguments != `{"label":"one"}` || !created || !completed {
		t.Fatalf("invalid client stream: %q", arguments)
	}
}
func TestAGYSessionOwnerAndConsumedParent(t *testing.T) {
	s := &session{owner: "alice:key-a", responses: map[string]bool{"resp_a": true}, pending: map[string]*pendingTool{}}
	m := &Manager{sessions: map[string]*session{"s": s}}
	if _, e := m.find("bob:key-b", request{Previous: "resp_a"}); e == nil {
		t.Fatal("cross-owner response was accepted")
	}
	if found, e := m.find("alice:key-a", request{Previous: "resp_a"}); e != nil || found != s {
		t.Fatal("owner could not continue")
	}
	s.responses["resp_a"] = false
	if _, e := m.find("alice:key-a", request{Previous: "resp_a"}); e == nil {
		t.Fatal("consumed parent was reused")
	}
}
func TestAGYToolResultCannotBeReplaced(t *testing.T) {
	call := &pendingTool{ID: "call_a", Delivered: true, Ready: make(chan struct{})}
	s := &session{pending: map[string]*pendingTool{call.ID: call}}
	result := []map[string]any{{"type": "function_call_output", "call_id": call.ID, "output": "first"}}
	if e := deliverTools(s, result); e != nil {
		t.Fatal(e)
	}
	result[0]["output"] = "different"
	if e := deliverTools(s, result); e == nil {
		t.Fatal("accepted a changed result for an executed tool")
	}
	if call.Output != "first" {
		t.Fatal("original result was overwritten")
	}
}
func TestAGYDisabledAccountIsFailure(t *testing.T) {
	e := agentFailure("\n⚠ This service has been disabled in this account for violation of Terms of Service. Please submit an appeal.")
	if e == nil || e.(*Error).Code != "account_disabled" {
		t.Fatal("provider service failure became success")
	}
	if agentFailure("The documentation mentions: This service has been disabled.") != nil {
		t.Fatal("ordinary answer was classified as an error")
	}
}
func TestAGYRejectsOpaqueContextAndOversizeWithoutTruncation(t *testing.T) {
	for _, item := range []map[string]any{{"type": "compaction", "encrypted_content": "opaque"}, {"role": "user", "content": strings.Repeat("x", MaxText+1)}} {
		if _, e := promptBlocks([]map[string]any{item}); e == nil {
			t.Fatal("unsupported context was silently accepted")
		}
	}
}
