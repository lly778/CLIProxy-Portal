package agy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitQueuedFollowups(t *testing.T, m *Manager, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.queuedFollowups.Load() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queued=%d want=%d", m.queuedFollowups.Load(), want)
}

func followupBody(items []map[string]any, messages ...string) string {
	items = copyItems(items)
	for _, text := range messages {
		items = append(items, map[string]any{"role": "user", "content": text})
	}
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "stream": true, "input": items})
	return string(body)
}

func TestAGYFollowupsQueueInOrderWithoutReplayingWork(t *testing.T) {
	var prompts []string
	m, s := recoveryFixture(t, func(target *session, f frame) any {
		if target.ID != "native" {
			t.Error("follow-up replaced the original native session")
		}
		prompts = append(prompts, string(f.Params))
		target.events <- agentEvent{Text: "queued answer"}
		return map[string]any{"stopReason": "end_turn"}
	})
	s.items = []map[string]any{{"role": "user", "content": "original task"}}
	s.history = requestFingerprints(s.items)
	s.running = true
	m.slot = make(chan struct{}, 1)
	m.slot <- struct{}{} // The original HTTP response owns the only slot.
	s.requestMu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			s.requestMu.Unlock()
		}
	})
	firstBody := followupBody(s.items, "why not finished")
	secondBody := followupBody(s.items, "why not finished", "continue the task")
	first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- recoveryRequest(m, firstBody) }()
	waitQueuedFollowups(t, m, 1)
	go func() { second <- recoveryRequest(m, secondBody) }()
	waitQueuedFollowups(t, m, 2)
	if len(m.slot) != 1 || s.life.Err() != nil {
		t.Fatal("queued messages consumed generation slots or cancelled original work")
	}
	// Finish the original response before allowing either follow-up to run.
	s.mu.Lock()
	s.items = append(s.items, map[string]any{"role": "assistant", "content": "original task finished"})
	s.history, s.running = requestFingerprints(s.items), false
	s.mu.Unlock()
	<-m.slot
	s.requestMu.Unlock()
	locked = false
	for _, done := range []chan *httptest.ResponseRecorder{first, second} {
		select {
		case w := <-done:
			if !strings.Contains(w.Body.String(), "queued_followup") || !strings.Contains(w.Body.String(), "queued answer") || strings.Contains(w.Body.String(), "response.failed") {
				t.Fatalf("queued request failed: %d %s", w.Code, w.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("follow-up did not run after original completion")
		}
	}
	if len(prompts) != 2 || !strings.Contains(prompts[0], "why not finished") || strings.Contains(prompts[0], "original task finished") || !strings.Contains(prompts[1], "continue the task") || strings.Contains(prompts[1], "why not finished") || m.queuedFollowups.Load() != 0 || s.life.Err() != nil {
		t.Fatalf("follow-up order/history incorrect: %+v", prompts)
	}
}

func TestAGYQueuedFollowupLetsToolResultsThroughOneSlot(t *testing.T) {
	var calls atomic.Int64
	var call *pendingTool
	m, s := recoveryFixture(t, func(target *session, f frame) any {
		if calls.Add(1) == 1 {
			select {
			case <-call.Ready:
			case <-target.life.Done():
				return map[string]any{"stopReason": "cancelled"}
			}
			target.mu.Lock()
			if call.Output != "actual file result" {
				t.Error("original tool result changed")
			}
			call.Returned = true
			target.mu.Unlock()
			target.events <- agentEvent{Text: "original task completed"}
		} else {
			if !strings.Contains(string(f.Params), "follow-up question") || strings.Contains(string(f.Params), "actual file result") {
				t.Error("follow-up replayed old results or lost the new question")
			}
			target.events <- agentEvent{Text: "follow-up reply"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	m.slot = make(chan struct{}, 1)
	fn := tool{Type: "function", Name: "read"}
	s.tools, s.advertisedTools = []tool{fn}, []tool{fn}
	s.items = []map[string]any{{"role": "user", "content": "read the file"}, {"type": "function_call", "call_id": "read-call", "name": "read", "arguments": "{}"}}
	s.history = requestFingerprints(s.items)
	s.responses["tool-response"] = true
	s.responses["previous"], s.requestPrevious = false, "previous"
	call = &pendingTool{ID: "read-call", Tool: fn, Delivered: true, Ready: make(chan struct{})}
	s.pending[call.ID] = call
	m.startPrompt(s, []any{map[string]string{"type": "text", "text": "original prompt"}})
	queued := make(chan *httptest.ResponseRecorder, 1)
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "stream": true, "input": "follow-up question"})
	go func() { queued <- recoveryRequest(m, string(body)) }()
	waitQueuedFollowups(t, m, 1)
	if len(m.slot) != 0 || len(s.tools) != 1 {
		t.Fatal("follow-up blocked the single tool-result slot or revoked tools")
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- recoveryRequest(m, `{"model":"agy/test","previous_response_id":"tool-response","input":[{"type":"function_call_output","call_id":"read-call","output":"actual file result"}]}`)
	}()
	for _, tc := range []struct {
		done chan *httptest.ResponseRecorder
		text string
	}{{result, "original task completed"}, {queued, "follow-up reply"}} {
		select {
		case w := <-tc.done:
			if !strings.Contains(w.Body.String(), tc.text) {
				t.Fatalf("result/follow-up failed: %d %s", w.Code, w.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("queued follow-up prevented the original tool from completing")
		}
	}
	if calls.Load() != 2 || s.ID != "native" || s.life.Err() != nil || len(s.tools) != 1 || m.queuedFollowups.Load() != 0 {
		t.Fatal("follow-up changed original work identity or tool availability")
	}
}

func TestAGYCancelledQueuedFollowupKeepsOriginalWork(t *testing.T) {
	var calls atomic.Int64
	m, s := recoveryFixture(t, func(*session, frame) any { calls.Add(1); return nil })
	s.items = []map[string]any{{"role": "user", "content": "original task"}}
	s.history, s.running = requestFingerprints(s.items), true
	before := digest(snapshotSession(s))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(followupBody(s.items, "follow up"))).WithContext(ctx)
		r.Header.Set("X-Portal-Owner", "owner")
		m.executeResponsesHTTP(httptest.NewRecorder(), r)
	}()
	waitQueuedFollowups(t, m, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled follow-up remained queued")
	}
	if m.queuedFollowups.Load() != 0 || len(m.slot) != 0 || calls.Load() != 0 || s.life.Err() != nil || digest(snapshotSession(s)) != before {
		t.Fatal("cancelling a queued follow-up modified the original task")
	}
}

func TestAGYFollowupQueueBoundDoesNotTakeToolSlots(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { t.Error("queued request started early"); return nil })
	m.slot = make(chan struct{}, 1)
	s.items = []map[string]any{{"role": "user", "content": "original task"}}
	s.history, s.running = requestFingerprints(s.items), true
	var cancels []context.CancelFunc
	var done []chan struct{}
	for i := 0; i < maxQueuedFollowups; i++ {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cancels = append(cancels, cancel)
		finished := make(chan struct{})
		done = append(done, finished)
		body := followupBody(s.items, "queued message")
		go func() {
			defer close(finished)
			r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(body)).WithContext(ctx)
			r.Header.Set("X-Portal-Owner", "owner")
			m.executeResponsesHTTP(httptest.NewRecorder(), r)
		}()
		waitQueuedFollowups(t, m, int64(i+1))
	}
	w := recoveryRequest(m, followupBody(s.items, "one too many"))
	if !strings.Contains(w.Body.String(), "followup_queue_full") || len(m.slot) != 0 || m.queuedFollowups.Load() != maxQueuedFollowups || s.life.Err() != nil {
		t.Fatalf("queue bound damaged original work or took tool slots: %s", w.Body.String())
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, finished := range done {
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("queued cancellation did not release capacity")
		}
	}
	if m.queuedFollowups.Load() != 0 || s.life.Err() != nil || !s.running {
		t.Fatal("queue cleanup cancelled original work or leaked capacity")
	}
}

func TestAGYFollowupQueueRespectsReservedPortalCapacity(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { t.Error("follow-up unexpectedly started"); return nil })
	s.items = []map[string]any{{"role": "user", "content": "original task"}}
	s.history, s.running = requestFingerprints(s.items), true
	before := digest(snapshotSession(s))
	r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(followupBody(s.items, "follow up")))
	r.Header.Set("X-Portal-Owner", "owner")
	r.Header.Set("X-Portal-Followup-Limit", "0")
	w := httptest.NewRecorder()
	m.executeResponsesHTTP(w, r)
	if !strings.Contains(w.Body.String(), "followup_queue_full") || m.queuedFollowups.Load() != 0 || len(m.slot) != 0 || s.life.Err() != nil || digest(snapshotSession(s)) != before {
		t.Fatal("queued follow-up consumed capacity reserved for the original tool results")
	}
}
