package agy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func replayTestRequest(ctx context.Context, body, id string) *http.Request {
	r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("X-Portal-Owner", "owner:key")
	if id != "" {
		r.Header.Set("Idempotency-Key", id)
	}
	return r
}

func TestAGYReplayIdentitiesSeparateOwnersConversationsAndChangedRequests(t *testing.T) {
	body := []byte(`{"model":"agy/test","input":"hello"}`)
	h := make(http.Header)
	if responseReplayKey("owner", h, body) != "" {
		t.Fatal("unidentified text was treated as a global duplicate")
	}
	h.Set("Session_id", "chat-a")
	h.Set("X-Codex-Turn-Metadata", `{"turn_id":"turn-a"}`)
	h.Set("X-Client-Request-Id", "trace-a")
	key := responseReplayKey("owner", h, body)
	h.Set("X-Client-Request-Id", "trace-b")
	if key == "" || key != responseReplayKey("owner", h, []byte(`{ "input": "hello", "model": "agy/test" }`)) {
		t.Fatal("transport trace changes or JSON formatting broke retry identity")
	}
	if key == responseReplayKey("different-owner", h, body) || key == responseReplayKey("owner", h, []byte(`{"model":"agy/test","input":"changed"}`)) {
		t.Fatal("owner or payload change reused a response")
	}
	h.Set("Session_id", "chat-b")
	if key == responseReplayKey("owner", h, body) {
		t.Fatal("independent chats share retry state")
	}
	h = make(http.Header)
	key = responseReplayKey("owner", h, []byte(`{"model":"agy/test","previous_response_id":"resp-a","input":"next"}`))
	if key == "" || key == responseReplayKey("owner", h, []byte(`{"model":"agy/test","previous_response_id":"resp-b","input":"next"}`)) {
		t.Fatal("response chains were not isolated")
	}
	h.Set("Idempotency-Key", "large-number")
	if responseReplayKey("owner", h, []byte(`{"n":9007199254740992}`)) == responseReplayKey("owner", h, []byte(`{"n":9007199254740993}`)) {
		t.Fatal("distinct large integer arguments collapsed")
	}
}

func TestAGYReplayReconnectsToRunningTaskAndKeepsResponseAndToolIDs(t *testing.T) {
	life, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := &Manager{life: life}
	var executions atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	execute := func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-original\"}}\n\n")
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			t.Error("client disconnect cancelled the original execution")
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-original\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call-original\",\"name\":\"read\",\"arguments\":\"{}\"}]}}\n\n")
	}
	ctx, detach := context.WithCancel(t.Context())
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		m.replayResponse(httptest.NewRecorder(), replayTestRequest(ctx, `{"model":"agy/test","input":"read"}`, "request-a"), execute)
	}()
	<-started
	detach()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("disconnected subscriber did not leave")
	}
	second := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		m.replayResponse(second, replayTestRequest(t.Context(), `{"input":"read","model":"agy/test"}`, "request-a"), execute)
	}()
	close(release)
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("retry did not finish with original execution")
	}
	if executions.Load() != 1 || !strings.Contains(second.Body.String(), "resp-original") || !strings.Contains(second.Body.String(), "call-original") || !strings.Contains(second.Body.String(), "response.completed") {
		t.Fatalf("retry restarted or lost original identifiers: calls=%d %s", executions.Load(), second.Body.String())
	}
}

func TestAGYReplayPersistsCompletedResponseAndAllowsFailedRetry(t *testing.T) {
	home, secret := t.TempDir(), strings.Repeat("s", 64)
	disk, err := openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.db.Close()
	m := &Manager{life: t.Context(), disk: disk}
	var calls atomic.Int64
	execute := func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp-saved","status":"completed","output":[{"type":"message","role":"assistant","content":"private-response"}]}`)
	}
	first := httptest.NewRecorder()
	m.replayResponse(first, replayTestRequest(t.Context(), `{"model":"agy/test","input":"hello"}`, "persisted"), execute)
	m.stopResponseReplays(false)
	m = &Manager{life: t.Context(), disk: disk}
	second := httptest.NewRecorder()
	m.replayResponse(second, replayTestRequest(t.Context(), `{"model":"agy/test","input":"hello"}`, "persisted"), execute)
	if calls.Load() != 1 || first.Body.String() != second.Body.String() {
		t.Fatal("completed response was regenerated after restart")
	}
	var payload []byte
	if err := disk.db.QueryRow(`SELECT payload FROM response_replays LIMIT 1`).Scan(&payload); err != nil || strings.Contains(string(payload), "private-response") {
		t.Fatal("response persistence was not encrypted", err)
	}
	m.clearResponseReplays()
	m.replayResponse(httptest.NewRecorder(), replayTestRequest(t.Context(), `{"model":"agy/test","input":"hello"}`, "persisted"), execute)
	if calls.Load() != 2 {
		t.Fatal("account reset reused the previous account's response")
	}
	var failures atomic.Int64
	flaky := func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(1) == 1 {
			WriteError(w, fail(503, "busy", "try later"))
			return
		}
		execute(w, r)
	}
	failed := httptest.NewRecorder()
	m.replayResponse(failed, replayTestRequest(t.Context(), `{"model":"agy/test","input":"retry"}`, "failed"), flaky)
	retry := httptest.NewRecorder()
	m.replayResponse(retry, replayTestRequest(t.Context(), `{"model":"agy/test","input":"retry"}`, "failed"), flaky)
	if failed.Code != 503 || retry.Code != 200 || failures.Load() != 2 {
		t.Fatalf("an actual failure blocked retry: %d %d calls=%d", failed.Code, retry.Code, failures.Load())
	}
}

func TestAGYRequestIdentityForwardingDoesNotForwardCallerSecrets(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 64) || r.Header.Get("Cookie") != "" || r.Header.Get("X-Untrusted") != "" {
			t.Error("caller secrets crossed the private worker boundary")
		}
		if r.Header.Get("Idempotency-Key") != "stable-request" || r.Header.Get("Session_id") != "chat-a" {
			t.Error("stable retry identity was dropped")
		}
		io.WriteString(w, `{}`)
	}))
	defer worker.Close()
	client, _ := NewClient(worker.URL, strings.Repeat("s", 64))
	h := make(http.Header)
	h.Set("Authorization", "Bearer private-client-key")
	h.Set("Cookie", "private-cookie")
	h.Set("X-Untrusted", "private")
	h.Set("Idempotency-Key", "stable-request")
	h.Set("Session_id", "chat-a")
	r, err := client.Request(WithRequestIdentity(t.Context(), h), "POST", "/internal/responses", "owner:key", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
}

func TestAGYRepeatedToolResultsReuseOriginalContinuation(t *testing.T) {
	var calls atomic.Int64
	m, s := recoveryFixture(t, func(s *session, f frame) any {
		calls.Add(1)
		s.events <- agentEvent{Text: "used the completed client tool result"}
		return map[string]any{"stopReason": "end_turn"}
	})
	c := &pendingTool{ID: "call-once", Tool: tool{Type: "function", Name: "write"}, Delivered: true, Relayed: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	const body = `{"model":"agy/test","previous_response_id":"previous","input":[{"type":"function_call_output","call_id":"call-once","output":"already written"}]}`
	first := recoveryRequest(m, body)
	second := recoveryRequest(m, body)
	if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() || calls.Load() != 1 || !c.Replayed {
		t.Fatalf("tool result continuation was repeated: calls=%d first=%s second=%s", calls.Load(), first.Body.String(), second.Body.String())
	}
}

func TestAGYReplaySeparatesCompactionFromGeneration(t *testing.T) {
	m := &Manager{life: t.Context()}
	var calls atomic.Int64
	execute := func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/internal/compact" {
			io.WriteString(w, `{"id":"compact-original","object":"response.compaction","output":[]}`)
		} else {
			io.WriteString(w, `{"id":"response-original","status":"completed","output":[]}`)
		}
	}
	for _, path := range []string{"/internal/responses", "/internal/compact", "/internal/compact"} {
		r := replayTestRequest(t.Context(), `{"model":"agy/test","input":"same input"}`, "same-request")
		r.URL.Path = path
		w := httptest.NewRecorder()
		m.replayResponse(w, r, execute)
		if w.Code != 200 || path == "/internal/compact" && !strings.Contains(w.Body.String(), "compact-original") {
			t.Fatalf("compaction reused a generation response: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatal("compaction retry was executed twice")
	}
}

func TestAGYReplayExecutionPanicAllowsRetry(t *testing.T) {
	m := &Manager{life: t.Context()}
	var calls atomic.Int64
	execute := func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			panic("fixture execution failure")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"response-retried","status":"completed","output":[]}`)
	}
	first, second := httptest.NewRecorder(), httptest.NewRecorder()
	m.replayResponse(first, replayTestRequest(t.Context(), `{"input":"hello"}`, "panic-retry"), execute)
	m.replayResponse(second, replayTestRequest(t.Context(), `{"input":"hello"}`, "panic-retry"), execute)
	if first.Code != 503 || second.Code != 200 || calls.Load() != 2 {
		t.Fatalf("execution panic prevented retry: %d %d calls=%d", first.Code, second.Code, calls.Load())
	}
}
