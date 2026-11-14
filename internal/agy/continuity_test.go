package agy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func continuityCall(s *session) *pendingTool {
	fn := tool{Type: "function", Name: "write", Parameters: map[string]any{"type": "object"}}
	s.tools, s.advertisedTools = []tool{fn}, []tool{fn}
	s.toolHash = digest(s.tools)
	s.items = []map[string]any{{"role": "user", "content": "original objective: update the specified file"}, {"type": "function_call", "name": "write", "arguments": "{}", "call_id": "original-call"}}
	s.history = requestFingerprints(s.items)
	c := &pendingTool{ID: "original-call", BatchID: "original-batch", Tool: fn, Arguments: map[string]any{}, Delivered: true, Relayed: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	return c
}

func TestResultOnlyContinuationKeepsOriginalNativeSessionAndFailureEvidence(t *testing.T) {
	var prompts atomic.Int64
	var actualPrompt string
	m, s := recoveryFixture(t, func(target *session, f frame) any {
		prompts.Add(1)
		if target.ID != "native" {
			t.Error("result-only continuation created a replacement session")
		}
		actualPrompt = string(f.Params)
		target.events <- agentEvent{Text: "continue using the actual result"}
		return map[string]any{"stopReason": "end_turn"}
	})
	c := continuityCall(s)
	body := `{"model":"agy/test","input":[{"type":"function_call_output","call_id":"original-call","output":{"isError":true,"content":[{"type":"text","text":"partial output"}],"structuredContent":{"record":9007199254740993}}},{"role":"user","content":"continue from this result"}]}`
	w := recoveryRequest(m, body)
	if w.Code != 200 || prompts.Load() != 1 || s.ID != "native" || !c.Replayed {
		t.Fatalf("result-only continuation lost its session: %d %s", w.Code, w.Body.String())
	}
	for _, want := range []string{"original-call", "partial output", "reported failure", "9007199254740993", "continue from this result"} {
		if !strings.Contains(actualPrompt, want) {
			t.Fatal("continuation lost result/steering", want, actualPrompt)
		}
	}
	if len(s.items) < 4 || s.items[0]["content"] != "original objective: update the specified file" {
		t.Fatal("result continuation discarded earlier objective")
	}
}

func TestRebuildValidatesPendingResultsBeforeCancellingOriginal(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { t.Error("validation ran a model prompt"); return nil })
	c := continuityCall(s)
	req := request{Model: s.model, Instructions: json.RawMessage(`"updated instructions"`), Items: copyItems(s.items)}
	before := digest(s.items)
	if _, err := m.rebuildSession(t.Context(), s, req); err == nil {
		t.Fatal("rebuild discarded an unresolved tool")
	}
	if s.life.Err() != nil || m.sessions[s.tenant] != s || digest(s.items) != before {
		t.Fatal("rejected rebuild mutated/cancelled original task")
	}
	req.Items = append(req.Items, map[string]any{"type": "custom_tool_call_output", "call_id": c.ID, "output": "wrong result kind"})
	if _, err := m.rebuildSession(t.Context(), s, req); err == nil {
		t.Fatal("rebuild bypassed original tool type validation")
	}
	if s.life.Err() != nil {
		t.Fatal("invalid result cancelled original waiter")
	}
	req.Items[len(req.Items)-1] = map[string]any{"type": "function_call_output", "call_id": c.ID, "output": "actual completion"}
	next, err := m.rebuildSession(t.Context(), s, req)
	if err != nil || next == nil || next.ID == s.ID {
		t.Fatal("validated rebuild failed", err)
	}
	select {
	case <-c.Ready:
		t.Fatal("validation woke the original MCP call")
	default:
	}
}

func TestCheckpointKeepsAcceptedResultsAfterMCPDeliveryOrContextPreparation(t *testing.T) {
	for _, state := range []string{"returned", "prepared"} {
		t.Run(state, func(t *testing.T) {
			_, s := recoveryFixture(t, func(*session, frame) any { return nil })
			c := continuityCall(s)
			s.running = true
			result := map[string]any{"type": "function_call_output", "call_id": c.ID, "output": map[string]any{"isError": false, "structuredContent": map[string]any{"record": json.Number("9007199254740993")}, "content": []any{}}}
			if err := deliverTools(s, []map[string]any{result}); err != nil {
				t.Fatal(err)
			}
			c.Returned, c.Replayed = state == "returned", state == "prepared"
			v := closingCheckpoint(s, nil)
			if len(v.WaitingTools) != 0 || !v.Interrupted {
				t.Fatal("consumed result revived a new execution")
			}
			if len(v.Items) != 3 || itemFingerprint(v.Items[2]) != itemFingerprint(result) || len(v.History) != len(v.Items) {
				t.Fatal("accepted result vanished before HTTP history commit")
			}
		})
	}
}

func TestToolBatchDiagnosticsRejectUnknownMixAndSurviveCheckpoint(t *testing.T) {
	log := &observationLog{}
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	m.cfg.Observe = log.save
	s.trace = &requestTrace{m: m, owner: "owner:key", model: s.model, request: "continuation", session: s.ID}
	a := continuityCall(s)
	b := &pendingTool{ID: "other-call", BatchID: a.BatchID, Tool: a.Tool, Delivered: true, Ready: make(chan struct{})}
	s.pending[b.ID] = b
	output := func(id string) map[string]any {
		return map[string]any{"type": "function_call_output", "call_id": id, "output": "actual result"}
	}
	if err := deliverTools(s, []map[string]any{output(a.ID)}); err == nil || !strings.Contains(err.Error(), b.ID) {
		t.Fatal("missing result did not identify the original call", err)
	}
	if err := deliverTools(s, []map[string]any{output(a.ID), output(b.ID), output("unrelated-call")}); err == nil {
		t.Fatal("unknown result was silently treated as delivered")
	}
	for _, c := range []*pendingTool{a, b} {
		select {
		case <-c.Ready:
			t.Fatal("invalid batch woke an original tool")
		default:
		}
	}
	out := newResponse(httptest.NewRecorder(), false, s.model)
	out.trace, out.batchID = s.trace, a.BatchID
	recordToolBatch(s, out)
	var manifest, diagnostic bool
	for _, event := range log.snapshot() {
		if event.Kind != "batch" || event.BatchID != a.BatchID {
			continue
		}
		manifest = manifest || event.State == "dispatched" && len(event.CallIDs) == 2
		diagnostic = diagnostic || event.State == "missing_results" && len(event.CallIDs) == 1 && event.CallIDs[0] == b.ID
	}
	if !manifest || !diagnostic {
		t.Fatal("batch evidence missing")
	}
	disk, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.db.Close()
	s.last = time.Now()
	if err = disk.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	loaded, err := disk.loadSession(s.ID, s.owner, 0)
	if err != nil || loaded == nil || loaded.pending[a.ID].BatchID != a.BatchID || loaded.pending[b.ID].BatchID != b.BatchID {
		t.Fatal("batch identity lost on recovery", err)
	}
	if err := deliverTools(s, []map[string]any{output(b.ID), output(a.ID)}); err != nil {
		t.Fatal("out of order results rejected", err)
	}
	if err := deliverTools(s, []map[string]any{output(a.ID), output(b.ID)}); err != nil {
		t.Fatal("identical result retry rejected", err)
	}
}

func TestNativeExecutionResultStatusRemainsPartOfHistory(t *testing.T) {
	success := map[string]any{"type": "apply_patch_call_output", "call_id": "call", "status": "completed", "output": "patch result"}
	failure := map[string]any{"type": "apply_patch_call_output", "call_id": "call", "status": "failed", "output": "patch result"}
	if itemFingerprint(success) == itemFingerprint(failure) {
		t.Fatal("failed patch became the same historical evidence as success")
	}
}

func TestToolFollowupAndDefinitionUpdateGoToAnUnreadResult(t *testing.T) {
	_, s := recoveryFixture(t, func(*session, frame) any { return nil })
	old := continuityCall(s)
	delete(s.pending, old.ID)
	old.ID = "z-old-call"
	s.pending[old.ID] = old
	s.items[1]["call_id"] = old.ID
	s.history = requestFingerprints(s.items)
	oldResult := map[string]any{"type": "function_call_output", "call_id": old.ID, "output": "previous result"}
	if err := deliverTools(s, []map[string]any{oldResult}); err != nil {
		t.Fatal(err)
	}
	old.Returned = true
	old.Extra = []any{map[string]string{"type": "text", "text": "previous steering"}}
	fresh := &pendingTool{ID: "a-fresh-call", BatchID: "fresh-batch", Tool: old.Tool, Delivered: true, Ready: make(chan struct{})}
	s.pending[fresh.ID] = fresh
	s.items = append(s.items, oldResult, map[string]any{"type": "function_call", "call_id": fresh.ID, "name": "write", "arguments": "{}"})
	s.history = requestFingerprints(s.items)
	items := append(copyItems(s.items), map[string]any{"type": "function_call_output", "call_id": fresh.ID, "output": "fresh result"}, map[string]any{"role": "user", "content": "critical new instruction"})
	req := request{ToolsProvided: true, Tools: append(append([]tool(nil), s.tools...), tool{Type: "function", Name: "new_tool"}), Items: items}
	if err := deliverToolsForRequest(s, items, &req); err != nil {
		t.Fatal(err)
	}
	forwarded, _ := json.Marshal(fresh.Extra)
	if !strings.Contains(string(forwarded), "critical new instruction") || !strings.Contains(fresh.Update, "new_tool") {
		t.Fatal("new message or tool update was attached to a consumed historical result")
	}
	prior, _ := json.Marshal(old.Extra)
	if strings.Contains(string(prior), "critical new instruction") || old.Update != "" {
		t.Fatal("already delivered result was rewritten")
	}
}

func TestRepeatedDeltaResultKeepsHistoryAndCanQueueNewMessage(t *testing.T) {
	_, s := recoveryFixture(t, func(*session, frame) any { return nil })
	c := continuityCall(s)
	result := map[string]any{"type": "function_call_output", "call_id": c.ID, "output": "actual result"}
	if err := deliverTools(s, []map[string]any{result}); err != nil {
		t.Fatal(err)
	}
	c.Returned = true
	s.items = append(s.items, result)
	s.history = requestFingerprints(s.items)
	req := request{Model: s.model, Items: []map[string]any{result, {"role": "user", "content": "next instruction"}}}
	if !mergeResultHistory(s, &req) || len(req.Items) != len(s.items)+1 || !canQueueFollowup(s, req) {
		t.Fatal("identical delta result blocked/lost a valid follow-up")
	}
	if req.Items[len(req.Items)-1]["content"] != "next instruction" {
		t.Fatal("new input disappeared")
	}
}

func TestUnforwardableNewInstructionsAreRejectedWithoutChangingState(t *testing.T) {
	_, s := recoveryFixture(t, func(*session, frame) any { return nil })
	c := continuityCall(s)
	result := map[string]any{"type": "function_call_output", "call_id": c.ID, "output": "actual result"}
	if err := deliverTools(s, []map[string]any{result}); err != nil {
		t.Fatal(err)
	}
	c.Returned = true
	before := digest(s.items)
	if err := deliverTools(s, []map[string]any{result, {"role": "user", "content": "must not be silently consumed"}}); err == nil {
		t.Fatal("new instruction was marked seen without a receiver")
	}
	if digest(s.items) != before || len(c.Extra) != 0 {
		t.Fatal("unforwardable request mutated authoritative state")
	}
}

func TestMissingOutputIsNotAnAcknowledgedNullResult(t *testing.T) {
	_, s := recoveryFixture(t, func(*session, frame) any { return nil })
	c := continuityCall(s)
	if err := deliverTools(s, []map[string]any{{"type": "function_call_output", "call_id": c.ID}}); err == nil {
		t.Fatal("missing output became a fabricated null result")
	}
	select {
	case <-c.Ready:
		t.Fatal("missing output woke original tool")
	default:
	}
	if err := deliverTools(s, []map[string]any{{"type": "function_call_output", "call_id": c.ID, "output": nil}}); err != nil {
		t.Fatal("explicit null output incorrectly rejected", err)
	}
}

func TestConcurrentColdRecoveryLoadsOneNativeSessionAndKeepsBindingReserved(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var loads atomic.Int64
	p := diskTestProcess(t, func(f frame) any {
		if f.Method == "session/load" {
			if loads.Add(1) == 1 {
				close(started)
				<-release
			}
			return map[string]any{"configOptions": []any{map[string]any{"id": "model", "options": []any{map[string]any{"value": "test"}}}}}
		}
		return map[string]any{}
	})
	m := &Manager{cfg: RuntimeConfig{MaxSessions: 3}, proc: p, life: t.Context(), sessions: map[string]*session{}, mcpURL: "http://127.0.0.1:1234/mcp/"}
	cold := func() *session {
		return &session{ID: "cold-native", owner: "owner:key", model: "agy/test", system: digest(""), responses: map[string]bool{"resp-cold": true}, last: time.Now()}
	}
	a, b := cold(), cold()
	req := request{Model: "agy/test", bindRequest: true}
	finished := make(chan error, 1)
	go func() { _, err := m.restoreNativeSession(t.Context(), a, req); finished <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("native recovery never started")
	}
	_, duplicate := m.restoreNativeSession(t.Context(), b, req)
	_, lookup := m.find(a.owner, request{Model: a.model, Previous: "resp-cold"})
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if duplicate == nil || lookup == nil || loads.Load() != 1 || b.life != nil || a.life.Err() != nil {
		t.Fatal("concurrent recovery replaced/mutated an active native session", duplicate, lookup, loads.Load())
	}
	if !a.loading {
		t.Fatal("cold session exposed before its first handler bound the response lock")
	}
	if _, err := m.restoreNativeSession(t.Context(), b, req); err == nil {
		t.Fatal("stale decoded record loaded again after recovery")
	}
	a.requestMu.Lock()
	a.mu.Lock()
	a.loading = false
	a.mu.Unlock()
	a.requestMu.Unlock()
	if found, err := m.find(a.owner, request{Model: a.model, Previous: "resp-cold"}); err != nil || found != a {
		t.Fatal("bound session could not be continued", err)
	}
}

func TestStaleColdLineageCannotRebuildAnAlreadyResumedConversation(t *testing.T) {
	current := &session{ID: "replacement-native", owner: "owner:key", responses: map[string]bool{"old-response": false}}
	m := &Manager{sessions: map[string]*session{"current": current}}
	stale := &session{ID: "old-native", owner: current.owner, responses: map[string]bool{"old-response": true}}
	if release, err := m.claimSessionRecovery(stale); err == nil {
		release()
		t.Fatal("stale disk state recreated an already resumed lineage")
	}
}
