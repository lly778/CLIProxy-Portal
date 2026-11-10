package agy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAGYCodexReasoningOrderPreservesStrictHistory(t *testing.T) {
	reason := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "same thought"}}}
	call := map[string]any{"type": "function_call", "call_id": "call", "name": "read", "arguments": `{"path":"a"}`}
	user := map[string]any{"role": "user", "content": "task"}
	items := []map[string]any{user, call, reason}
	canonical := canonicalHistoryItems(items)
	if !reflect.DeepEqual(canonical, []map[string]any{user, reason, call}) || items[1]["type"] != "function_call" {
		t.Fatal("reasoning order was not normalized without mutating the request")
	}
	s := &session{items: canonical, history: requestFingerprints(canonical)}
	for _, field := range []string{"arguments", "call_id", "name", "summary"} {
		changed := copyItems(canonical)
		if field == "summary" {
			changed[1][field] = []any{map[string]any{"type": "summary_text", "text": "different thought"}}
		} else {
			changed[2][field] = "different"
		}
		if prefix(s.history, requestFingerprints(changed)) {
			t.Fatalf("changed %s matched the history", field)
		}
	}
	result := map[string]any{"type": "function_call_output", "call_id": "call", "output": "done"}
	for _, boundary := range []map[string]any{user, result, {"type": "message", "role": "assistant", "content": "visible answer"}} {
		separated := []map[string]any{call, boundary, reason}
		if !reflect.DeepEqual(canonicalHistoryItems(separated), separated) {
			t.Fatal("reasoning crossed a semantic boundary")
		}
	}
}

func TestAGYCodexReorderedReasoningContinuesOversizedHistoryIncrementally(t *testing.T) {
	var prompts []string
	m, s := recoveryFixture(t, func(target *session, f frame) any {
		if target.ID != "native" {
			t.Error("tool result rebuilt the original native session")
		}
		prompts = append(prompts, string(f.Params))
		target.events <- agentEvent{Text: "continued"}
		return map[string]any{"stopReason": "end_turn"}
	})
	read := tool{Type: "function", Name: "read", Parameters: map[string]any{"type": "object"}}
	s.tools, s.advertisedTools = []tool{read}, []tool{read}
	s.toolHash = digest(s.tools)
	s.items = []map[string]any{
		{"role": "user", "content": strings.Repeat("old context ", MaxText/12+100)},
		{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "same thought"}}},
		{"type": "function_call", "call_id": "call", "name": "read", "arguments": "{}"},
	}
	s.history = requestFingerprints(s.items)
	c := &pendingTool{ID: "call", Tool: read, Delivered: true, Relayed: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	items := copyItems(s.items)
	items[1]["content"] = nil
	items[1]["encrypted_content"] = nil
	items[1], items[2] = items[2], items[1]
	items = append(items, map[string]any{"type": "function_call_output", "call_id": "call", "output": "small new result"})
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "input": items})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || len(prompts) != 1 || !strings.Contains(prompts[0], "small new result") || strings.Contains(prompts[0], "old context") || len(prompts[0]) > 4096 {
		t.Fatalf("tool continuation replayed history: status=%d prompts=%d response=%s", w.Code, len(prompts), w.Body.String())
	}
	if !c.Replayed {
		t.Fatal("actual tool result was not consumed")
	}
	items = copyItems(s.items)
	items = append(items, map[string]any{"role": "user", "content": "next small message"})
	body, _ = json.Marshal(map[string]any{"model": "agy/test", "input": items})
	w = recoveryRequest(m, string(body))
	if w.Code != 200 || len(prompts) != 2 || !strings.Contains(prompts[1], "next small message") || strings.Contains(prompts[1], "old context") || strings.Contains(prompts[1], "small new result") {
		t.Fatalf("next message did not reuse the original session: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYCodexReasoningOrderFindsParkedSession(t *testing.T) {
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	items := []map[string]any{{"role": "user", "content": "task"}, {"type": "reasoning", "summary": []any{}}, {"type": "function_call", "call_id": "call", "name": "read", "arguments": "{}"}}
	v := diskTestRecord("reason-order", "owner:key", time.Now())
	v.Items, v.History = items, requestFingerprints(items)
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	input := copyItems(items)
	input[1], input[2] = input[2], input[1]
	body, _ := json.Marshal(map[string]any{"model": v.Model, "input": input})
	r, err := parseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.find(v.Owner, r, nil)
	if err != nil || got == nil || got.ID != v.ID {
		t.Fatal("reordered reasoning lost the parked session", err)
	}
}

func TestAGYCodexReasoningOrderReturnsResultToWaitingMCP(t *testing.T) {
	m, s := recoveryFixture(t, func(_ *session, _ frame) any {
		t.Error("waiting MCP result started a replacement prompt")
		return map[string]any{"stopReason": "end_turn"}
	})
	read := tool{Type: "function", Name: "read", Parameters: map[string]any{"type": "object"}}
	s.tools, s.advertisedTools = []tool{read}, []tool{read}
	s.toolHash = digest(s.tools)
	s.items = []map[string]any{{"role": "user", "content": "task"}, {"type": "reasoning", "summary": []any{}}, {"type": "function_call", "call_id": "call", "name": "read", "arguments": "{}"}}
	s.history = requestFingerprints(s.items)
	s.running = true
	c := &pendingTool{ID: "call", Tool: read, Delivered: true, Relayed: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	go func() {
		select {
		case <-c.Ready:
			s.mu.Lock()
			c.Returned = true
			s.mu.Unlock()
			s.events <- agentEvent{Text: "continued in the waiting turn"}
			s.events <- agentEvent{Result: &promptResult{Stop: "end_turn"}}
		case <-t.Context().Done():
		}
	}()
	items := copyItems(s.items)
	items[1]["content"] = nil
	items[1]["encrypted_content"] = nil
	items[1], items[2] = items[2], items[1]
	items = append(items, map[string]any{"type": "function_call_output", "call_id": "call", "output": "real result"})
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "input": items})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "continued in the waiting turn") || c.Output != "real result" {
		t.Fatalf("MCP continuation lost the original turn: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYCodexReasoningNullMetadataPreservesContentChecks(t *testing.T) {
	base := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "same thought"}}}
	next := copyItems([]map[string]any{base})[0]
	next["content"], next["encrypted_content"] = nil, nil
	if itemFingerprint(base) != itemFingerprint(next) {
		t.Fatal("Codex null optional reasoning fields changed the history")
	}
	for _, field := range []string{"content", "encrypted_content"} {
		changed := copyItems([]map[string]any{next})[0]
		changed[field] = "actual new data"
		if itemFingerprint(base) == itemFingerprint(changed) {
			t.Fatalf("non-null %s was ignored", field)
		}
	}
}
