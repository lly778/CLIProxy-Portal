package agy

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

const codexCompactFixture = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume from this checkpoint."

func TestAGYCodexCompactionResumesOriginalToolWithResultAndKeepsCatalog(t *testing.T) {
	var prompts atomic.Int64
	var original *session
	var call *pendingTool
	var delivered string
	m, source := recoveryFixture(t, func(target *session, f frame) any {
		prompts.Add(1)
		select {
		case <-call.Ready:
		case <-target.life.Done():
			return map[string]any{"stopReason": "cancelled"}
		}
		target.mu.Lock()
		if target != original || target.internal || target.policy.Mode != "none" || len(target.tools) != 1 {
			t.Error("checkpoint replaced the native session or revoked its catalog")
		}
		b, _ := json.Marshal(call.Extra)
		delivered = string(b)
		if call.Output != "real file content" {
			t.Error("checkpoint lost the actual tool result")
		}
		call.Returned = true
		target.mu.Unlock()
		target.events <- agentEvent{Text: "checkpoint with actual result"}
		return map[string]any{"stopReason": "end_turn"}
	})
	original = source
	fn := tool{Type: "function", Name: "read", Parameters: map[string]any{"type": "object"}}
	source.tools, source.advertisedTools = []tool{fn}, []tool{fn}
	source.toolHash = digest(source.tools)
	source.policy = toolPolicy{Mode: "auto"}
	source.system = digest("original work instructions")
	source.items = []map[string]any{
		{"role": "user", "content": "inspect the project"},
		{"type": "function_call", "call_id": "read-call", "name": "read", "arguments": "{}"},
	}
	source.history = requestFingerprints(source.items)
	call = &pendingTool{ID: "read-call", Tool: fn, Delivered: true, Ready: make(chan struct{})}
	source.pending[call.ID] = call
	m.startPrompt(source, []any{map[string]string{"type": "text", "text": "original work prompt"}})
	body, _ := json.Marshal(map[string]any{
		"model": "agy/test", "previous_response_id": "previous", "tools": []any{}, "stream": true,
		"instructions": "different summary-only system instructions",
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": call.ID, "output": "real file content"},
			map[string]any{"role": "user", "content": codexCompactFixture},
		},
	})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "checkpoint with actual result") || !strings.Contains(w.Body.String(), `"type":"response.completed"`) || prompts.Load() != 1 {
		t.Fatalf("checkpoint failed: %d %s prompts=%d", w.Code, w.Body.String(), prompts.Load())
	}
	if !strings.Contains(delivered, "CONTEXT CHECKPOINT COMPACTION") || !strings.Contains(delivered, "Do not call any tool") {
		t.Fatalf("original tool did not carry summary instruction and temporary policy: %s", delivered)
	}
	if source.ID != "native" || source.system != digest("original work instructions") || len(source.tools) != 1 || source.policy.Mode != "auto" || source.checkpointing || source.life.Err() != nil || len(m.sessions) != 1 || m.sessions[source.tenant] != source {
		t.Fatal("checkpoint changed work identity, catalog, restored policy or session lifetime")
	}
	source.mu.Lock()
	nextCall, fresh := parkToolCall(source, fn.mcpName(), map[string]any{})
	source.mu.Unlock()
	if nextCall == nil || !fresh {
		t.Fatal("original tool could not be called after the summary restriction ended")
	}
}

func TestAGYCodexCompactionMissingBatchResultsLeavesCallsUntouched(t *testing.T) {
	var prompt string
	m, source := recoveryFixture(t, func(target *session, f frame) any {
		prompt = string(f.Params)
		target.events <- agentEvent{Text: "summary after all results"}
		return map[string]any{"stopReason": "end_turn"}
	})
	fn := tool{Type: "function", Name: "read"}
	source.tools, source.advertisedTools = []tool{fn}, []tool{fn}
	source.policy = toolPolicy{Mode: "auto"}
	source.items = []map[string]any{
		{"role": "user", "content": "inspect"},
		{"type": "function_call", "call_id": "a", "name": "read", "arguments": "{}"},
		{"type": "function_call", "call_id": "b", "name": "read", "arguments": "{}"},
	}
	source.history = requestFingerprints(source.items)
	for _, id := range []string{"a", "b"} {
		source.pending[id] = &pendingTool{ID: id, Tool: fn, Delivered: true, Ready: make(chan struct{})}
	}
	before := digest(snapshotSession(source))
	input := []any{map[string]any{"type": "function_call_output", "call_id": "a", "output": "result a"}, map[string]any{"role": "user", "content": codexCompactFixture}}
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "tools": []any{}, "input": input})
	w := recoveryRequest(m, string(body))
	if !strings.Contains(w.Body.String(), "missing_tool_result") || prompt != "" || digest(snapshotSession(source)) != before || source.life.Err() != nil || source.checkpointing {
		t.Fatalf("incomplete batch mutated work: %d %s", w.Code, w.Body.String())
	}
	for _, c := range source.pending {
		select {
		case <-c.Ready:
			t.Fatal("partial result woke a tool")
		default:
		}
	}
	input = append(input[:1], map[string]any{"type": "function_call_output", "call_id": "b", "output": "result b"}, map[string]any{"role": "user", "content": codexCompactFixture})
	body, _ = json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "tools": []any{}, "input": input})
	w = recoveryRequest(m, string(body))
	if !strings.Contains(w.Body.String(), "summary after all results") || !strings.Contains(prompt, "result a") || !strings.Contains(prompt, "result b") || !strings.Contains(prompt, "CONTEXT CHECKPOINT COMPACTION") || source.ID != "native" || len(source.tools) != 1 || source.policy.Mode != "auto" {
		t.Fatalf("complete batch failed to summarize in original session: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYCodexCompactionDoesNotRestoreAnUnownedResponse(t *testing.T) {
	var calls atomic.Int64
	m, _ := recoveryFixture(t, func(*session, frame) any { calls.Add(1); return nil })
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "unowned", "tools": []any{}, "input": codexCompactFixture})
	w := recoveryRequest(m, string(body))
	if !strings.Contains(w.Body.String(), "session_expired") || calls.Load() != 0 {
		t.Fatalf("checkpoint bypassed response ownership: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYCodexCompactionUnsafeTurnDoesNotConsumeOrCancelWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		call *pendingTool
		code string
	}{
		{"still generating", nil, "compaction_turn_running"},
		{"undelivered call", &pendingTool{ID: "call", Ready: make(chan struct{})}, "compaction_tools_pending"},
		{"result being delivered", &pendingTool{ID: "call", Delivered: true, Replying: 1, Ready: make(chan struct{})}, "compaction_tools_pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prompts atomic.Int64
			m, source := recoveryFixture(t, func(*session, frame) any { prompts.Add(1); return nil })
			source.items = []map[string]any{{"role": "user", "content": "work in progress"}}
			source.history = requestFingerprints(source.items)
			source.running = true
			source.tools = []tool{{Type: "function", Name: "read"}}
			if tc.call != nil {
				source.pending[tc.call.ID] = tc.call
			}
			before := digest(snapshotSession(source))
			body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "tools": []any{}, "input": codexCompactFixture})
			w := recoveryRequest(m, string(body))
			if !strings.Contains(w.Body.String(), tc.code) || prompts.Load() != 0 || digest(snapshotSession(source)) != before || source.life.Err() != nil || source.checkpointing || len(m.sessions) != 1 {
				t.Fatalf("unsafe checkpoint changed original work: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAGYCodexCompactionRewrittenHistoryLeavesOriginalToolUntouched(t *testing.T) {
	var prompts atomic.Int64
	m, source := recoveryFixture(t, func(*session, frame) any { prompts.Add(1); return nil })
	source.items = []map[string]any{{"role": "user", "content": "original task"}, {"type": "function_call", "name": "read", "call_id": "call", "arguments": "{}"}}
	source.history = requestFingerprints(source.items)
	source.running = true
	call := &pendingTool{ID: "call", Delivered: true, Ready: make(chan struct{})}
	source.pending[call.ID] = call
	before := digest(snapshotSession(source))
	items := copyItems(source.items)
	items[0]["content"] = "different task"
	items = append(items, map[string]any{"type": "function_call_output", "call_id": "call", "output": "result"}, map[string]any{"role": "user", "content": codexCompactFixture})
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "tools": []any{}, "input": items})
	w := recoveryRequest(m, string(body))
	if !strings.Contains(w.Body.String(), "compaction_history_changed") || prompts.Load() != 0 || digest(snapshotSession(source)) != before || source.life.Err() != nil {
		t.Fatalf("rewritten checkpoint consumed original tool: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-call.Ready:
		t.Fatal("rewritten history woke the original tool")
	default:
	}
}

func TestAGYCodexCompactionOmittedReasoningStillMatchesActualHistory(t *testing.T) {
	var prompt string
	m, source := recoveryFixture(t, func(target *session, f frame) any {
		if target.ID != "native" {
			t.Error("reasoning projection selected a different native session")
		}
		prompt = string(f.Params)
		target.events <- agentEvent{Text: "checkpoint from existing context"}
		return map[string]any{"stopReason": "end_turn"}
	})
	source.items = []map[string]any{
		{"role": "user", "content": "original task"},
		{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "original reasoning summary"}}},
		{"type": "function_call", "call_id": "original-call", "name": "read", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "original-call", "output": "actual result"},
	}
	source.history = requestFingerprints(source.items)
	source.tools = []tool{{Type: "function", Name: "read"}}
	items := []map[string]any{source.items[0], source.items[2], source.items[3], {"role": "user", "content": codexCompactFixture}}
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "tools": []any{}, "input": items})
	w := recoveryRequest(m, string(body))
	if !strings.Contains(w.Body.String(), "checkpoint from existing context") || !strings.Contains(prompt, "CONTEXT CHECKPOINT COMPACTION") || strings.Contains(prompt, "actual result") || len(source.tools) != 1 || source.items[1]["type"] != "reasoning" {
		t.Fatalf("omitted reasoning broke original-session compaction: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYCodexCompactionAllowsExistingAwaitAndCompletionUpdates(t *testing.T) {
	m, source := recoveryFixture(t, func(*session, frame) any { return nil })
	source.checkpointing, source.running = true, true
	source.policy = toolPolicy{Mode: "none"}
	source.reported["existing"] = reportedTool{Server: bridgeName, Name: "read"}
	retryTestUpdate(m, source, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "existing", "status": "completed"})
	retryTestUpdate(m, source, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "await", "name": "mcp_portal_" + awaitName, "status": "in_progress"})
	if len(source.events) != 0 || source.life.Err() != nil {
		t.Fatal("checkpoint blocked an existing tool completion or its result waiter")
	}
}

func TestAGYCodexCompactionCannotInvokeClientOrOfficialTools(t *testing.T) {
	var m *Manager
	m, source := recoveryFixture(t, func(target *session, _ frame) any {
		retryTestUpdate(m, target, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "forbidden", "name": "search_web", "status": "in_progress"})
		return map[string]any{"stopReason": "end_turn"}
	})
	source.items = []map[string]any{{"role": "user", "content": "original task"}}
	source.history = requestFingerprints(source.items)
	source.tools = []tool{{Type: "function", Name: "read"}}
	source.policy = toolPolicy{Mode: "auto"}
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "tools": []any{}, "input": codexCompactFixture})
	w := recoveryRequest(m, string(body))
	if !strings.Contains(w.Body.String(), "compaction_tool_denied") || strings.Contains(w.Body.String(), `"type":"web_search_call"`) || len(source.tools) != 1 || source.policy.Mode != "auto" {
		t.Fatalf("checkpoint executed a tool or revoked work catalog: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYOrdinaryEmptyToolsStillRevokesLiveTools(t *testing.T) {
	m, s := recoveryFixture(t, func(target *session, _ frame) any {
		target.events <- agentEvent{Text: "continuation after the actual result"}
		return map[string]any{"stopReason": "end_turn"}
	})
	fn := tool{Type: "function", Name: "read"}
	s.tools, s.advertisedTools = []tool{fn}, []tool{fn}
	s.running = true
	c := &pendingTool{ID: "call", Tool: fn, Delivered: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	s.events <- agentEvent{Text: "tools revoked as requested"}
	s.events <- agentEvent{Result: &promptResult{Stop: "end_turn"}}
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","tools":[],"input":[{"type":"function_call_output","call_id":"call","output":"done"},{"role":"user","content":"Continue without tools."}]}`)
	if !strings.Contains(w.Body.String(), "tools revoked as requested") || len(s.tools) != 0 {
		t.Fatalf("ordinary revocation was ignored: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYCodexCompactionRecognitionIsLimitedToFinalCheckpointRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input any
		tools []any
		want  bool
	}{
		{"plain", codexCompactFixture, []any{}, true},
		{"parts", []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": codexCompactFixture}}}}, []any{}, true},
		{"normal revocation", "Do not use tools anymore.", []any{}, false},
		{"quoted marker", "Explain this text: " + codexCompactFixture, []any{}, false},
		{"incomplete marker", "You are performing a CONTEXT CHECKPOINT COMPACTION. Please explain the phrase.", []any{}, false},
		{"old checkpoint", []any{map[string]any{"role": "user", "content": codexCompactFixture}, map[string]any{"role": "user", "content": "Continue editing."}}, []any{}, false},
		{"tool result", []any{map[string]any{"type": "function_call_output", "call_id": "old", "output": codexCompactFixture}}, []any{}, false},
		{"tools still enabled", codexCompactFixture, []any{map[string]any{"type": "function", "name": "read"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"model": "agy/test", "input": tc.input, "tools": tc.tools})
			r, err := parseRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			if got := isCodexCompactionRequest(r); got != tc.want {
				t.Fatalf("checkpoint recognition=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestAGYCodexCompactionLongHistoryOnlySendsNewInstructionToOriginalSession(t *testing.T) {
	var nativePrompt string
	var prompts atomic.Int64
	m, source := recoveryFixture(t, func(target *session, f frame) any {
		prompts.Add(1)
		if target.ID != "native" || target.internal || target.policy.Mode != "none" || len(target.tools) != 1 {
			t.Error("long checkpoint replaced work or revoked tools")
		}
		nativePrompt = string(f.Params)
		target.events <- agentEvent{Text: "final bounded checkpoint"}
		return map[string]any{"stopReason": "end_turn"}
	})
	var chunks int
	m.summarizeTest = func(_ context.Context, _ string, _ []any, _ func()) (string, error) {
		chunks++
		return "precompressed", nil
	}
	source.tools = []tool{{Type: "function", Name: "read"}}
	source.policy = toolPolicy{Mode: "auto"}
	source.items = []map[string]any{{"role": "user", "content": "inspect the project"}, {"role": "assistant", "content": strings.Repeat("earlier evidence ", 28000)}}
	source.history = requestFingerprints(source.items)
	items := append(copyItems(source.items), map[string]any{"role": "user", "content": codexCompactFixture})
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "tools": []any{}, "input": items})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "final bounded checkpoint") || chunks != 0 || prompts.Load() != 1 || strings.Contains(nativePrompt, "earlier evidence") || !strings.Contains(nativePrompt, "CONTEXT CHECKPOINT COMPACTION") || len(nativePrompt) > MaxText {
		t.Fatalf("large history was replayed or pre-compacted: %d %s chunks=%d prompts=%d bytes=%d", w.Code, w.Body.String(), chunks, prompts.Load(), len(nativePrompt))
	}
	if source.ID != "native" || source.life.Err() != nil || len(source.tools) != 1 || source.policy.Mode != "auto" || source.checkpointing || len(m.sessions) != 1 {
		t.Fatal("long checkpoint changed work identity, catalog or lifetime")
	}
}

func TestAGYCodexCompactionUnknownSessionDoesNotOpenOne(t *testing.T) {
	var calls atomic.Int64
	m, _ := recoveryFixture(t, func(*session, frame) any { calls.Add(1); return nil })
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "tools": []any{}, "input": codexCompactFixture})
	w := recoveryRequest(m, string(body))
	if !strings.Contains(w.Body.String(), "compaction_session_unavailable") || calls.Load() != 0 || len(m.sessions) != 1 {
		t.Fatalf("missing source created a summary session: %d %s", w.Code, w.Body.String())
	}
}

func TestAGYOrdinaryLongHistoryStillUsesCompaction(t *testing.T) {
	var nativePrompt string
	m, _ := recoveryFixture(t, func(target *session, f frame) any {
		nativePrompt = string(f.Params)
		target.events <- agentEvent{Text: "continued ordinary task"}
		return map[string]any{"stopReason": "end_turn"}
	})
	var chunks int
	m.summarizeTest = func(_ context.Context, _ string, _ []any, _ func()) (string, error) {
		chunks++
		return "earlier evidence summarized", nil
	}
	body, _ := json.Marshal(map[string]any{
		"model": "agy/test", "tools": []any{},
		"input": []any{
			map[string]any{"role": "user", "content": "inspect the project"},
			map[string]any{"role": "assistant", "content": strings.Repeat("earlier evidence ", 28000)},
			map[string]any{"role": "user", "content": "Continue the task."},
		},
	})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "continued ordinary task") || chunks == 0 || !strings.Contains(nativePrompt, "earlier evidence summarized") || strings.Contains(nativePrompt, strings.Repeat("earlier evidence ", 100)) {
		t.Fatalf("ordinary work bypassed compaction: %d %s chunks=%d", w.Code, w.Body.String(), chunks)
	}
}

func TestAGYCodexCompactionRestoresSameColdNativeSession(t *testing.T) {
	var prompt string
	m, source := recoveryFixture(t, func(target *session, f frame) any {
		if target.ID != "native" || target.internal || target.policy.Mode != "none" || len(target.tools) != 1 {
			t.Error("cold checkpoint replaced original native ID or catalog")
		}
		prompt = string(f.Params)
		target.events <- agentEvent{Text: "cold checkpoint"}
		return map[string]any{"stopReason": "end_turn"}
	})
	disk, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.db.Close() })
	m.disk = disk
	source.items = []map[string]any{{"role": "user", "content": "saved project evidence"}}
	source.history = requestFingerprints(source.items)
	source.tools = []tool{{Type: "function", Name: "read"}}
	source.officialVersion = officialToolsVersion
	if err = disk.put(snapshotSession(source), true); err != nil {
		t.Fatal(err)
	}
	delete(m.sessions, source.tenant)
	body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "instructions": "summary-only instructions", "tools": []any{}, "input": codexCompactFixture})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "cold checkpoint") || strings.Contains(prompt, "saved project evidence") || !strings.Contains(prompt, "CONTEXT CHECKPOINT COMPACTION") || len(m.sessions) != 1 {
		t.Fatalf("cold checkpoint failed to resume original context: %d %s", w.Code, w.Body.String())
	}
	for _, live := range m.sessions {
		if live.ID != "native" || live.internal || live.checkpointing || len(live.tools) != 1 {
			t.Fatal("cold checkpoint lost native identity or catalog")
		}
	}
}
