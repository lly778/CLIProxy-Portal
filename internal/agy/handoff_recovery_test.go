package agy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func recoveryOutput(t *testing.T, w *httptest.ResponseRecorder) (string, []map[string]any) {
	t.Helper()
	var response struct {
		ID     string
		Output []map[string]any
	}
	if w.Code != 200 || unmarshalExact(w.Body.Bytes(), &response) != nil || response.ID == "" {
		t.Fatal("recovery response failed", w.Code, w.Body.String())
	}
	return response.ID, response.Output
}

func TestAutomaticHandoffWarmRecoveryKeepsNextFullHistoryAndColdResume(t *testing.T) {
	var prompts atomic.Int64
	var inputs []string
	fn := tool{Type: "function", Name: "write_file", Parameters: map[string]any{"type": "object"}}
	args := map[string]any{"path": "fixture.docx"}
	m, source := recoveryFixture(t, func(target *session, f frame) any {
		n := prompts.Add(1)
		inputs = append(inputs, string(f.Params))
		if n == 1 {
			c := &pendingTool{ID: "original-write", Tool: fn, Arguments: args, Ready: make(chan struct{})}
			target.mu.Lock()
			target.pending[c.ID] = c
			target.mu.Unlock()
			target.events <- agentEvent{Call: c}
		} else {
			if target.ID != "native" {
				t.Error("recovery replaced the original native session")
			}
			target.mu.Lock()
			blocked := uncertainRecoveryAction(target, fn, args)
			target.mu.Unlock()
			if !blocked {
				t.Error("unknown original write was no longer protected")
			}
			target.events <- agentEvent{Text: "unknown operation skipped; dependent work deferred, independent work can continue"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	d := protocolDisk(t, m)
	source.officialVersion = officialToolsVersion
	source.tools, source.advertisedTools = []tool{fn}, []tool{fn}
	source.toolHash = digest(source.tools)
	body := `{"model":"agy/test","previous_response_id":"previous","input":"unique original write instruction"}`
	recoveryOutput(t, identifiedRecoveryRequest(m, body, "request-original"))
	var count int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("normal handoff added a full checkpoint", err)
	}
	if err := m.clearResponseReplays(); err != nil {
		t.Fatal(err)
	}
	_, output := recoveryOutput(t, identifiedRecoveryRequest(m, body, "request-original"))
	if prompts.Load() != 2 || !strings.Contains(inputs[1], "execution UNKNOWN") || !strings.Contains(inputs[1], "Inspect each recorded outcome") || strings.Contains(inputs[1], "unique original write instruction") {
		t.Fatal("recovery replayed the task or omitted outcome instructions", inputs[1])
	}
	for _, want := range []string{"without retrying them or launching verification calls", "skipped does not mean completed", "defer steps that depend", "identify the skipped operations", "user explicitly requests verification"} {
		if !strings.Contains(inputs[1], want) {
			t.Fatal("automatic recovery lost the default skip/dependency/reporting policy", want, inputs[1])
		}
	}
	client := []map[string]any{{"role": "user", "content": "unique original write instruction"}}
	client = append(client, output...)
	client = append(client, map[string]any{"role": "user", "content": "continue other authorized work"})
	next, _ := json.Marshal(map[string]any{"model": source.model, "input": client})
	_, output = recoveryOutput(t, identifiedRecoveryRequest(m, string(next), "next-turn"))
	if prompts.Load() != 3 || !strings.Contains(inputs[2], "continue other authorized work") || strings.Contains(inputs[2], "unique original write instruction") {
		t.Fatal("next full-history request lost its continuation", inputs[2])
	}
	var resident *session
	for _, s := range m.sessions {
		resident = s
	}
	if resident == nil || len(resident.handoffRecoveries) != 1 {
		t.Fatal("automatic recovery attempt was not bounded")
	}
	unknownCount := 0
	for _, item := range resident.items {
		if isRecoveryUnknown(item["output"]) {
			unknownCount++
			if receiptOutcome(item["output"]) != "unknown" {
				t.Fatal("skipped action was converted into a confirmed outcome")
			}
		}
	}
	if unknownCount != 1 {
		t.Fatal("default skip discarded the original unknown evidence", unknownCount)
	}
	// An interrupted recovery must not inject the older verify-first policy.
	resident.interruptedOutput = []map[string]any{{"role": "assistant", "content": "partial independent work"}}
	note := interruptedResumeNote(resident)
	if !strings.Contains(note, "without retrying them or launching verification calls") || strings.Contains(note, "verify any uncertain action before proceeding") {
		t.Fatal("interrupted continuation restored a conflicting verification policy", note)
	}
	if err := m.archiveSession(t.Context(), resident); err != nil {
		t.Fatal(err)
	}
	delete(m.sessions, resident.tenant)
	client = append(client, output...)
	client = append(client, map[string]any{"role": "user", "content": "continue after ordinary archival"})
	next, _ = json.Marshal(map[string]any{"model": source.model, "input": client})
	recoveryOutput(t, identifiedRecoveryRequest(m, string(next), "cold-next-turn"))
	if prompts.Load() != 4 || !strings.Contains(inputs[3], "continue after ordinary archival") {
		t.Fatal("archived continuation lost recovery context", inputs[3])
	}
}

func TestAutomaticHandoffPreservesSuccessFailureAndMissingResultSeparately(t *testing.T) {
	var prompt string
	m, s := recoveryFixture(t, func(target *session, f frame) any {
		prompt = string(f.Params)
		target.events <- agentEvent{Text: "reviewed recorded outcomes; uncertain work remains open"}
		return map[string]any{"stopReason": "end_turn"}
	})
	d := protocolDisk(t, m)
	body := `{"model":"agy/test","previous_response_id":"previous","input":"finish the task"}`
	r := identifiedReplayFixture(body, "mixed-status")
	key := m.handoffRequestKey(s.owner, r.Header, []byte(body))
	s.requestReplayKey, s.requestInputItems = key, 1
	s.officialVersion = officialToolsVersion
	s.items = []map[string]any{{"role": "user", "content": "finish the task"}}
	var known []map[string]any
	for _, id := range []string{"a-success", "b-failed", "c-missing"} {
		c := &pendingTool{ID: id, requestKey: key, Tool: tool{Type: "function", Name: id, Parameters: map[string]any{"type": "object"}}, Arguments: map[string]any{}, Delivered: true, Ready: make(chan struct{})}
		s.pending[id] = c
		s.items = append(s.items, pendingCheckpointItem(c))
		if id != "c-missing" {
			value := map[string]any{"isError": id == "b-failed", "content": []any{map[string]any{"type": "text", "text": id + " actual receipt"}}, "structuredContent": map[string]any{"record": json.Number("9007199254740993")}}
			c.Output, c.OutputHash = value, digest(value)
			c.ResultItem = map[string]any{"type": "function_call_output", "call_id": id, "output": value}
			close(c.Ready)
			known = append(known, c.ResultItem)
		}
	}
	s.history = requestFingerprints(s.items)
	s.waitAt = time.Now()
	if err := d.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	recoveryOutput(t, identifiedRecoveryRequest(m, body, "mixed-status"))
	var recovered *session
	for _, candidate := range m.sessions {
		recovered = candidate
	}
	if recovered == nil {
		t.Fatal("recovery did not retain the native session")
	}
	actual, unknown := 0, 0
	for _, item := range recovered.items {
		if !isToolResultItem(stringField(item, "type")) {
			continue
		}
		if isRecoveryUnknown(item["output"]) {
			unknown++
		} else {
			for _, original := range known {
				if itemFingerprint(original) == itemFingerprint(item) {
					actual++
				}
			}
		}
	}
	if actual != 2 || unknown != 1 || !strings.Contains(prompt, "a-success actual receipt") || !strings.Contains(prompt, "b-failed actual receipt") || !strings.Contains(prompt, "9007199254740993") {
		t.Fatal("recovery rewrote real outcomes or fabricated acknowledgements", actual, unknown, prompt)
	}
}

func identifiedReplayFixture(body, id string) *http.Request {
	r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(body))
	r.Header.Set("X-Portal-Owner", "owner")
	r.Header.Set("Idempotency-Key", id)
	return r
}

func TestFailedVerificationResponseCanContinueWithAnotherUserMessage(t *testing.T) {
	var prompts atomic.Int64
	var finalPrompt string
	m, source := recoveryFixture(t, func(target *session, f frame) any {
		n := prompts.Add(1)
		if n < 3 {
			name := "write_document"
			if n == 2 {
				name = "read_verification_status"
			}
			c := &pendingTool{ID: name, Tool: tool{Type: "function", Name: name, Parameters: map[string]any{"type": "object"}}, Arguments: map[string]any{}, Ready: make(chan struct{})}
			target.mu.Lock()
			target.pending[c.ID] = c
			target.mu.Unlock()
			target.events <- agentEvent{Call: c}
			if n == 2 {
				return map[string]any{"stopReason": "cancelled"}
			}
		} else {
			finalPrompt = string(f.Params)
			target.events <- agentEvent{Text: "continuing the conversation despite inconclusive verification"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	protocolDisk(t, m)
	source.officialVersion = officialToolsVersion
	body := `{"model":"agy/test","previous_response_id":"previous","stream":true,"input":"save the document"}`
	first := identifiedRecoveryRequest(m, body, "verification-failure")
	if !strings.Contains(first.Body.String(), "response.completed") {
		t.Fatal("initial handoff failed", first.Body.String())
	}
	if err := m.clearResponseReplays(); err != nil {
		t.Fatal(err)
	}
	failed := identifiedRecoveryRequest(m, body, "verification-failure")
	tracker := &Tracker{Stream: true}
	tracker.Feed(failed.Body.Bytes())
	tracker.Finish()
	if tracker.ResponseID == "" || !strings.Contains(failed.Body.String(), "response.failed") || prompts.Load() != 2 {
		t.Fatal("fixture did not fail its verification turn", failed.Body.String())
	}
	if cold, err := m.find(source.owner, request{Previous: tracker.ResponseID}); err != nil || cold == nil || len(cold.failedRecoveryResponses) == 0 {
		t.Fatal("failed recovery response lost its continuation identity", err)
	}
	next, _ := json.Marshal(map[string]any{"model": source.model, "previous_response_id": tracker.ResponseID, "input": "skip that verification and continue independent work"})
	recoveryOutput(t, identifiedRecoveryRequest(m, string(next), "continue-after-failure"))
	if prompts.Load() != 3 || !strings.Contains(finalPrompt, "skip that verification") || !strings.Contains(finalPrompt, "execution UNKNOWN") || !strings.Contains(finalPrompt, "agent_incomplete") {
		t.Fatal("failure locked the conversation or lost unknown evidence", finalPrompt)
	}
}

func automaticRetryFixture(t *testing.T) (*Manager, *session, request) {
	t.Helper()
	m, s := recoveryFixture(t, func(*session, frame) any { t.Error("validation invoked inference"); return nil })
	protocolDisk(t, m)
	body := `{"model":"agy/test","previous_response_id":"previous","input":"original request"}`
	r, err := parseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	r.owner = s.owner
	r.replayKey = m.handoffRequestKey(s.owner, identifiedReplayFixture(body, "validate-only").Header, []byte(body))
	s.requestReplayKey, s.requestInputItems = r.replayKey, 1
	s.officialVersion = officialToolsVersion
	c := &pendingTool{ID: "original-operation", requestKey: r.replayKey, Tool: tool{Type: "function", Name: "write", Parameters: map[string]any{"type": "object"}}, Arguments: map[string]any{}, Delivered: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	s.items = []map[string]any{{"role": "user", "content": "original request"}, pendingCheckpointItem(c)}
	s.history = requestFingerprints(s.items)
	s.waitAt = time.Now()
	return m, s, r
}

func TestAutomaticRecoveryRejectsUnsafeStateWithoutChangingOriginal(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*session, *request)
	}{
		{"different_model", func(_ *session, r *request) { r.Model = "agy/other" }},
		{"different_system", func(_ *session, r *request) { r.Instructions = json.RawMessage(`"changed instructions"`) }},
		{"official_inflight", func(s *session, _ *request) { s.officialActive = map[string]bool{"official": true} }},
		{"remote_inflight", func(s *session, _ *request) { s.remoteInFlight = true }},
		{"interrupted_without_loadable_wait", func(s *session, _ *request) { s.interrupted = true }},
		{"another_request_advanced", func(s *session, _ *request) { s.requestReplayKey = strings.Repeat("a", 64) }},
		{"already_attempted", func(s *session, r *request) { s.handoffRecoveries = []string{r.replayKey} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s, r := automaticRetryFixture(t)
			tc.edit(s, &r)
			before := digest(s.items)
			if _, err := m.prepareHandoffRetry(s.owner, &r); err == nil {
				t.Fatal("unsafe state was automatically taken over")
			}
			if digest(s.items) != before || s.life.Err() != nil || s.pending["original-operation"].Output != nil {
				t.Fatal("rejected recovery changed the original state")
			}
		})
	}
}

func TestAutomaticRecoveryBusyAndArchiveFailureKeepOriginalWait(t *testing.T) {
	for _, state := range []string{"request_lock", "queued_followup", "active_delivery", "save_failure"} {
		t.Run(state, func(t *testing.T) {
			m, s, r := automaticRetryFixture(t)
			if _, err := m.prepareHandoffRetry(s.owner, &r); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "request_lock":
				s.requestMu.Lock()
				defer s.requestMu.Unlock()
			case "queued_followup":
				s.followups = []*queuedFollowup{{}}
			case "active_delivery":
				s.pending["original-operation"].traceRequest = "old-origin"
				m.replays = map[string]*responseReplay{"old": {originRequest: "old-origin", readers: 1}}
			case "save_failure":
				if _, err := m.disk.db.Exec(`CREATE TRIGGER reject_archive BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			before := digest(s.items)
			if _, err := m.coldDetachedSession(t.Context(), s, &r); err == nil {
				t.Fatal("unsafe or unpersisted wait was cancelled")
			}
			if digest(s.items) != before || s.life.Err() != nil || s.loading || s.pending["original-operation"].Output != nil || len(s.handoffRecoveries) != 0 {
				t.Fatal("failed recovery lost a waiter or consumed its only attempt")
			}
		})
	}
}

func TestRecoverySetupFailureKeepsResponseIdentityForLaterContinuation(t *testing.T) {
	m, s, _ := automaticRetryFixture(t)
	m.sessions = map[string]*session{} // Simulate a cold candidate that failed to load.
	out := newResponse(httptest.NewRecorder(), true, s.model)
	out.continuableFailure = true
	out.onFailure = func() { m.linkRecoveryFailure(s, out) }
	if err := out.event("response.created", map[string]any{"response": out.envelope("in_progress", Usage{})}); err != nil {
		t.Fatal(err)
	}
	out.fail(fail(503, "session_restore_failed", "temporary restore failure"))
	if cold, err := m.find(s.owner, request{Previous: out.id}); err != nil || cold == nil || len(cold.failedRecoveryResponses) != 1 || cold.failedRecoveryResponses[0] != out.id {
		t.Fatal("setup failure orphaned the response seen by the client", err)
	}
}

func TestRecoveryStateAdvanceNeverFallsBackToReexecutingOldInput(t *testing.T) {
	m, s, _ := automaticRetryFixture(t)
	changed := false
	m.cfg.Observe = func(v Observation) error {
		if v.Kind == "phase" && v.State == "waiting_generation_slot" {
			s.mu.Lock()
			s.pending = map[string]*pendingTool{}
			s.items = append(s.items, map[string]any{"role": "assistant", "content": "concurrent completion"})
			s.history = requestFingerprints(s.items)
			s.mu.Unlock()
			changed = true
		}
		return nil
	}
	w := identifiedRecoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"original request"}`, "validate-only")
	if !changed || w.Code != 409 || !strings.Contains(w.Body.String(), "followup_history_changed") {
		t.Fatal("stale recovery fell through to ordinary task execution", w.Code, w.Body.String())
	}
}
