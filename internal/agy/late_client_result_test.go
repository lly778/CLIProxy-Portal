package agy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func lateClientFixture(t *testing.T, kind string, onPrompt func(*session, frame) any) (*Manager, *session, map[string]any) {
	t.Helper()
	m, s := recoveryFixture(t, onPrompt)
	m.cfg.Secret = strings.Repeat("s", 64)
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.db.Close() })
	m.disk = d
	fn := tool{Type: kind, Name: "save_document", Parameters: map[string]any{"type": "object"}}
	args := map[string]any{"path": "fixture.docx", "record": json.Number("9007199254740993")}
	if kind == "custom" {
		args = map[string]any{"input": "the actual patch"}
	}
	c := &pendingTool{ID: "recovered-save", Tool: fn, Arguments: args, Delivered: true, Ready: make(chan struct{})}
	base := cacheMatchItems()
	s.tools, s.advertisedTools = []tool{fn}, []tool{fn}
	s.toolHash, s.officialVersion = digest(s.tools), officialToolsVersion
	s.items = append(copyItems(base), pendingCheckpointItem(c))
	s.history = requestFingerprints(s.items)
	s.pending, s.running = map[string]*pendingTool{c.ID: c}, true
	r := request{Model: s.model, Items: append(copyItems(base), map[string]any{"role": "user", "content": "skip the missing result and continue independent work"})}
	if err := m.prepareDetachedResume(s, &r); err != nil {
		t.Fatal(err)
	}
	s.items = append(r.Items, map[string]any{"role": "assistant", "content": "unknown operation skipped"})
	s.history, s.running, c.Replayed = requestFingerprints(s.items), false, true
	return m, s, map[string]any{"type": stringField(c.ResultItem, "type"), "call_id": c.ID, "output": map[string]any{"isError": false, "structuredContent": map[string]any{"record": json.Number("9007199254740993")}, "content": []any{map[string]any{"type": "text", "text": "actual late save receipt"}, map[string]any{"type": "image", "mimeType": "image/png", "data": "YQ=="}}}}
}

func lateClientBody(t *testing.T, previous string, items ...map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": previous, "input": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestRecoveredLateReceiptKeepsUnknownEvidenceAndDoesNotRepeat(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, failed := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "Success", true: "Failure"}[failed], func(t *testing.T) {
				var prompts atomic.Int64
				var prompt string
				m, s, actual := lateClientFixture(t, kind, func(s *session, f frame) any {
					prompts.Add(1)
					prompt = string(f.Params)
					s.events <- agentEvent{Text: "received the real receipt"}
					return map[string]any{"stopReason": "end_turn"}
				})
				actual["output"].(map[string]any)["isError"] = failed
				before := requestFingerprints(s.items)
				args := s.pending["recovered-save"].Arguments
				if !uncertainRecoveryAction(s, s.tools[0], args) {
					t.Fatal("fixture did not protect the uncertain operation")
				}
				w := recoveryRequest(m, lateClientBody(t, "previous", actual, map[string]any{"role": "user", "content": "use the receipt; do not run the save again"}))
				if w.Code != 200 || prompts.Load() != 1 || !strings.Contains(w.Body.String(), "real receipt") || s.ID != "native" {
					t.Fatalf("late receipt lost original session: %d %s", w.Code, w.Body.String())
				}
				if !prefix(before, s.history) || !strings.Contains(prompt, "9007199254740993") || !strings.Contains(prompt, "image/png") || !strings.Contains(prompt, "supersedes") || strings.Contains(prompt, "actual contents") {
					t.Fatal("late receipt lost evidence/images/precision, rewrote UNKNOWN, or replayed old history", prompt)
				}
				if uncertainRecoveryAction(s, s.tools[0], args) {
					t.Fatal("acknowledged receipt still treated as absent")
				}
				h, err := inspectToolHistory(s.items)
				if err != nil || len(h.Records) != 3 {
					t.Fatal("invalid corrected tool history", err)
				}
				record := h.Records[2]
				outcome := map[bool]string{false: "success", true: "failed"}[failed]
				if record.Outcome != outcome || record.Provenance != "client_late_result" || itemFingerprint(record.Result) != itemFingerprint(actual) {
					t.Fatal("effective result is not the actual receipt", record)
				}
				// Bypass the response replay layer, as after its TTL or cache loss.
				duplicate := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(lateClientBody(t, "previous", actual)))
				req.Header.Set("X-Portal-Owner", "owner")
				m.executeResponsesHTTP(duplicate, req)
				if duplicate.Code != 200 || prompts.Load() != 1 || !strings.Contains(duplicate.Body.String(), `"status":"completed"`) {
					t.Fatalf("duplicate receipt triggered inference: %d %s", duplicate.Code, duplicate.Body.String())
				}
				changed := copyItems([]map[string]any{actual})[0]
				changed["output"] = "changed result"
				unchanged := digest(snapshotSession(s))
				rejected := recoveryRequest(m, lateClientBody(t, "previous", changed))
				if rejected.Code != 409 || !strings.Contains(rejected.Body.String(), "result_changed") || digest(snapshotSession(s)) != unchanged || prompts.Load() != 1 {
					t.Fatal("a real result was overwritten or retried", rejected.Body.String())
				}
				if m.disk.count.Load() != 0 {
					t.Fatal("normal receipt handling wrote a full session snapshot")
				}
			})
		}
	}
}

func TestRecoveredLateReceiptRejectsInvalidBatchBeforeMutation(t *testing.T) {
	for _, mode := range []string{"unknown_id", "wrong_type", "server_source", "invalid_source", "missing_output", "fake_unknown", "bad_image", "duplicate_id", "assistant", "history_edit", "edited_call_arguments", "old_real_output", "system", "model", "owner", "busy", "pending", "queued", "changed_before_commit"} {
		t.Run(mode, func(t *testing.T) {
			m, s, actual := lateClientFixture(t, "function", func(*session, frame) any { t.Error("invalid receipt started inference"); return nil })
			r := request{Model: s.model, owner: s.owner, Items: []map[string]any{actual}}
			x := copyItems([]map[string]any{actual})[0]
			r.Items = []map[string]any{x}
			switch mode {
			case "unknown_id":
				x["call_id"] = "unowned"
				r.Items = append([]map[string]any{actual}, x)
			case "wrong_type":
				x["type"] = "custom_tool_call_output"
			case "server_source":
				x["execution"] = "server"
			case "invalid_source":
				x["execution"] = 123
			case "missing_output":
				delete(x, "output")
			case "fake_unknown":
				x["output"] = recoveryUnknownResult()
			case "bad_image":
				x["output"] = []any{map[string]any{"type": "image", "mimeType": "text/html", "data": "bad"}}
			case "assistant":
				r.Items = append(r.Items, map[string]any{"role": "assistant", "content": "untrusted"})
			case "duplicate_id":
				r.Items = append(r.Items, actual)
			case "history_edit":
				r.Items = append(copyItems(s.items), actual)
				r.Items[0]["content"] = "rewritten old instruction"
			case "edited_call_arguments":
				r.Items = copyItems(restoredLateHistory(s, actual))
				for _, item := range r.Items {
					if item["type"] == "function_call" && resultCallID(item) == "recovered-save" {
						item["arguments"] = `{"path":"different.docx"}`
					}
				}
			case "old_real_output":
				r.Items = append(r.Items, map[string]any{"type": "function_call_output", "call_id": "old-read", "output": "rewritten old real output"})
			case "system":
				r.Instructions = json.RawMessage(`"changed"`)
			case "model":
				r.Model = "agy/other"
			case "owner":
				r.owner = "other:key"
			case "busy":
				s.running = true
			case "pending":
				s.pending["new-call"] = &pendingTool{ID: "new-call", Delivered: true, Ready: make(chan struct{})}
			case "queued":
				s.followups = []*queuedFollowup{{}}
			case "changed_before_commit":
				r.Items = r.Items[:1]
				if err := prepareLateClientResults(s, &r); err != nil {
					t.Fatal(err)
				}
				s.items = append(s.items, map[string]any{"role": "assistant", "content": "another response advanced"})
				s.history, s.running = requestFingerprints(s.items), true
			}
			before := digest(snapshotSession(s))
			if err := prepareLateClientResults(s, &r); err == nil || digest(snapshotSession(s)) != before || m.disk.count.Load() != 0 {
				t.Fatal("invalid batch changed original state", err)
			}
		})
	}
}

func TestRecoveredLateReceiptColdIndexFullHistoryAndNextPendingCall(t *testing.T) {
	var prompts, loads, news atomic.Int64
	m, s, actual := lateClientFixture(t, "function", func(*session, frame) any {
		t.Error("old process used")
		return &rpcError{Code: -32000, Message: "unexpected old process"}
	})
	before := copyItems(s.items)
	if err := m.disk.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	m.proc = diskTestProcess(t, func(f frame) any {
		switch f.Method {
		case "session/load":
			loads.Add(1)
		case "session/new":
			news.Add(1)
		case "session/prompt":
			prompts.Add(1)
			m.mu.Lock()
			for _, current := range m.sessions {
				current.events <- agentEvent{Text: "continued after late receipt"}
			}
			m.mu.Unlock()
			return map[string]any{"stopReason": "end_turn"}
		}
		return map[string]any{"sessionId": s.ID, "configOptions": s.options}
	})
	// A result-only request has no shared history or client header to route by.
	w := recoveryRequest(m, lateClientBody(t, "", actual))
	if w.Code != 200 || loads.Load() != 1 || news.Load() != 0 || prompts.Load() != 1 {
		t.Fatalf("cold result route failed: load=%d new=%d prompts=%d %d %s", loads.Load(), news.Load(), prompts.Load(), w.Code, w.Body.String())
	}
	for _, current := range m.sessions {
		s = current
	}
	if !prefix(requestFingerprints(before), s.history) {
		t.Fatal("cold correction lost the original history")
	}
	client, ok := recoveryProjection(s.items, s.recoveryOverlays)
	if !ok {
		t.Fatal("invalid projected history")
	}
	client = append(client, map[string]any{"role": "user", "content": "next full history request"})
	if w := recoveryRequest(m, lateClientBody(t, "", client...)); w.Code != 200 || prompts.Load() != 2 || news.Load() != 0 {
		t.Fatalf("next projected history rebuilt: %d %s", w.Code, w.Body.String())
	}
	// A later ordinary tool result must not be mistaken for another late patch
	// merely because the full transcript includes the previously patched result.
	c := &pendingTool{ID: "later-read", Tool: tool{Type: "function", Name: "read"}, Delivered: true, Ready: make(chan struct{})}
	s.pending[c.ID] = c
	s.items = append(s.items, pendingCheckpointItem(c))
	s.history = requestFingerprints(s.items)
	client, _ = recoveryProjection(s.items, s.recoveryOverlays)
	client = append(client, map[string]any{"type": "function_call_output", "call_id": c.ID, "output": "new actual read"})
	if w := recoveryRequest(m, lateClientBody(t, "", client...)); w.Code != 200 || prompts.Load() != 3 || news.Load() != 0 || !c.Replayed {
		t.Fatalf("normal subsequent tool result broken: %d %s", w.Code, w.Body.String())
	}
	if err := m.archiveSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	delete(m.sessions, s.tenant)
	// Confirmed result retransmission still gets an ACK after cold reload.
	duplicate := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(lateClientBody(t, "", actual)))
	req.Header.Set("X-Portal-Owner", "owner")
	m.executeResponsesHTTP(duplicate, req)
	if w := duplicate; w.Code != 200 || prompts.Load() != 3 || news.Load() != 0 || loads.Load() != 2 {
		t.Fatalf("cold duplicate ran inference: %d %s", w.Code, w.Body.String())
	}
}

func TestRecoveredLateReceiptIndexUpgradeIsolationAndAmbiguity(t *testing.T) {
	m, s, actual := lateClientFixture(t, "function", func(*session, frame) any { return nil })
	v := closingCheckpoint(s, nil)
	// Old response has been consumed but still belongs to this session.
	v.Responses["previous"] = false
	if err := m.disk.put(v, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.disk.db.Exec(`DELETE FROM session_late_result_routes; UPDATE cache_meta SET value=? WHERE id=2`, []byte("exact-numbers-recovery-overlays-v5")); err != nil {
		t.Fatal(err)
	}
	if err := m.disk.upgradeHistoryIndex(); err != nil {
		t.Fatal(err)
	}
	r := request{Model: s.model, Previous: "previous", Items: []map[string]any{actual}}
	if found, err := m.disk.find(s.owner, r, nil); err != nil || found == nil || found.ID != s.ID {
		t.Fatal("v5 upgrade or consumed response routing failed", err)
	}
	for _, mode := range []string{"owner", "model", "unowned_previous", "call_id"} {
		x, owner := r, s.owner
		switch mode {
		case "owner":
			owner = "another:key"
		case "model":
			x.Model = "agy/other"
		case "unowned_previous":
			x.Previous = "not-owned"
		case "call_id":
			x.Items = copyItems(r.Items)
			x.Items[0]["call_id"] = "different-call"
		}
		if found, err := m.disk.find(owner, x, nil); err != nil || found != nil {
			t.Fatal("late receipt crossed identity boundary", mode, err)
		}
	}
	var plaintext int
	if err := m.disk.db.QueryRow(`SELECT COUNT(*) FROM session_late_result_routes WHERE owner=? OR model=? OR call_key=?`, s.owner, s.model, resultCallID(actual)).Scan(&plaintext); err != nil || plaintext != 0 {
		t.Fatal("late receipt routing identity stored in plaintext", err)
	}
	other := v
	other.ID, other.Responses = "another-native", map[string]bool{"another-response": true}
	if err := m.disk.put(other, true); err != nil {
		t.Fatal(err)
	}
	r.Previous = ""
	if _, err := m.disk.find(s.owner, r, nil); err == nil {
		t.Fatal("ambiguous original call was silently selected")
	}
	if _, err := m.disk.db.Exec(`DELETE FROM sessions WHERE id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.disk.db.QueryRow(`SELECT COUNT(*) FROM session_late_result_routes WHERE session_id=?`, other.ID).Scan(&plaintext); err != nil || plaintext != 0 {
		t.Fatal("late result route survived parent deletion", err)
	}
}

func TestRecoveredLateReceiptCompactionArchivesActualResult(t *testing.T) {
	m, s, actual := lateClientFixture(t, "function", func(s *session, f frame) any {
		s.events <- agentEvent{Text: "received"}
		return map[string]any{"stopReason": "end_turn"}
	})
	unknown := s.pending["recovered-save"].ResultItem
	if w := recoveryRequest(m, lateClientBody(t, "", actual)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	m.summarizeTest = func(_ context.Context, _ string, _ []any, _ func()) (string, error) {
		return "summary without save detail", nil
	}
	items := append(copyItems(s.items), map[string]any{"role": "assistant", "content": strings.Repeat("filler ", 30000)}, map[string]any{"role": "user", "content": "continue independent work"})
	result, err := m.compactWithProof(t.Context(), request{owner: s.owner, Model: s.model, Items: items}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := m.previousToolRecords(s.owner, s.model, result.Items)
	if err != nil || len(refs) != 1 {
		t.Fatal("missing corrected result archive", err)
	}
	read := m.readToolRecord(t.Context(), s.owner, s.model, map[string]any{"reference": refs[0], "call_id": "recovered-save"})
	b, _ := json.Marshal(read)
	cap, err := m.parseToolRecordReference(s.owner, s.model, refs[0])
	if err != nil {
		t.Fatal(err)
	}
	archive, err := m.getToolRecords(t.Context(), cap)
	if err != nil || len(archive.Records) != 3 || archive.Records[2].Provenance != "client_late_result" || archive.Records[2].Outcome != "success" || !strings.Contains(string(b), "actual late save receipt") || !strings.Contains(string(b), "9007199254740993") {
		t.Fatal("compaction did not retain the real late receipt", string(b))
	}
	h, err := inspectToolHistory(s.items)
	if err != nil || h.Records[2].ResultIndex <= h.Records[2].CallIndex || reflect.DeepEqual(h.Records[2].Result, unknown) {
		t.Fatal("tool chain correction did not preserve both evidence states", err)
	}
	// The old signed archive is immutable; new archives carry current facts.
	if len(m.toolRecords) != 0 {
		// Disk-backed archives must not be copied into an unbounded memory map.
		t.Fatal("unexpected in-memory archive")
	}
}

func restoredLateHistory(s *session, actual map[string]any) []map[string]any {
	var items []map[string]any
	for _, item := range copyItems(s.items) {
		if item["type"] == lateClientResultType {
			continue
		}
		if isRecoveryUnknown(item["output"]) && resultCallID(item) == resultCallID(actual) {
			item = actual
		}
		items = append(items, item)
	}
	return items
}

func TestRecoveredLateReceiptFullHistoryRetainingOriginalCall(t *testing.T) {
	var prompts atomic.Int64
	var prompt string
	m, s, actual := lateClientFixture(t, "function", func(s *session, f frame) any {
		prompts.Add(1)
		prompt = string(f.Params)
		s.events <- agentEvent{Text: "use actual receipt"}
		return map[string]any{"stopReason": "end_turn"}
	})
	items := restoredLateHistory(s, actual)
	before := requestFingerprints(s.items)
	if w := recoveryRequest(m, lateClientBody(t, "", items...)); w.Code != 200 || s.ID != "native" || prompts.Load() != 1 || !prefix(before, s.history) {
		t.Fatalf("full original-call history failed: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(prompt, "actual contents") || !strings.Contains(prompt, "actual late save receipt") {
		t.Fatal("full transcript was replayed instead of a correction", prompt)
	}
	items = append(restoredLateHistory(s, actual), map[string]any{"role": "user", "content": "new task after receipt"})
	if w := recoveryRequest(m, lateClientBody(t, "", items...)); w.Code != 200 || prompts.Load() != 2 || s.ID != "native" {
		t.Fatalf("next restored full history failed: %d %s", w.Code, w.Body.String())
	}
	// Pure history matching, including disk lookup, must work without treating
	// old acknowledged results as novel receipts on every subsequent turn.
	if err := m.disk.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	items = append(restoredLateHistory(s, actual), map[string]any{"role": "user", "content": "next cold request"})
	r := request{Model: s.model, Items: items}
	if found, err := m.disk.find(s.owner, r, nil); err != nil || found == nil || found.ID != s.ID {
		t.Fatal("cold original-call history was not matched", err)
	}
	if carriesRecoveredResult(s, items) || !prefix(s.history, requestFingerprints(expandRecoveryHistory(s, items))) {
		t.Fatal("old corrected receipt became new input again")
	}
}

func TestRecoveredLateReceiptCannotForgeCorrectionOrCreateNewTask(t *testing.T) {
	m, s, actual := lateClientFixture(t, "function", func(*session, frame) any { t.Error("unowned result ran inference"); return nil })
	for _, mode := range []string{"owner", "model", "call", "previous", "internal_type"} {
		body := lateClientBody(t, "", actual)
		owner := s.owner
		switch mode {
		case "owner":
			owner = "other:key"
		case "model":
			body = strings.Replace(body, "agy/test", "agy/other", 1)
		case "call":
			body = strings.Replace(body, "recovered-save", "unknown-call", 1)
		case "previous":
			body = lateClientBody(t, "not-owned", actual)
		case "internal_type":
			body = lateClientBody(t, "", map[string]any{"type": lateClientResultType, "call_id": "recovered-save", "result": actual})
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(body))
		r.Header.Set("X-Portal-Owner", owner)
		before := digest(snapshotSession(s))
		m.executeResponsesHTTP(w, r)
		if w.Code < 400 || digest(snapshotSession(s)) != before || len(m.sessions) != 1 || m.disk.count.Load() != 0 {
			t.Fatalf("unowned receipt changed original state: %s %d %s", mode, w.Code, w.Body.String())
		}
	}
}

func TestRecoveredLateReceiptDoesNotInventSuccessFromPlainText(t *testing.T) {
	m, s, actual := lateClientFixture(t, "function", func(s *session, f frame) any {
		s.events <- agentEvent{Text: "receipt is inconclusive; skip the uncertain action"}
		return map[string]any{"stopReason": "end_turn"}
	})
	args := s.pending["recovered-save"].Arguments
	actual["output"] = "partial diagnostic with no verifiable execution outcome"
	if w := recoveryRequest(m, lateClientBody(t, "", actual)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	h, err := inspectToolHistory(s.items)
	if err != nil || h.Records[2].Outcome != "unknown" || h.Records[2].Provenance != "client_late_result" || !uncertainRecoveryAction(s, s.tools[0], args) {
		t.Fatal("inconclusive receipt was treated as successful or allowed repetition", err)
	}
}

func TestRecoveredLateReceiptCodexCheckpointUsesCorrectedEvidence(t *testing.T) {
	var prompts atomic.Int64
	m, s, actual := lateClientFixture(t, "function", func(s *session, f frame) any {
		prompts.Add(1)
		s.events <- agentEvent{Text: "checkpoint using acknowledged save receipt"}
		return map[string]any{"stopReason": "end_turn"}
	})
	if w := recoveryRequest(m, lateClientBody(t, "", actual)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	items := append(restoredLateHistory(s, actual), map[string]any{"role": "user", "content": codexCompactFixture})
	body, _ := json.Marshal(map[string]any{"model": s.model, "input": items, "tools": []any{}})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || prompts.Load() != 2 || s.ID != "native" || !strings.Contains(w.Body.String(), "client_late_result") || !strings.Contains(w.Body.String(), "success") {
		t.Fatalf("Codex checkpoint lost corrected evidence or rebuilt: %d %s", w.Code, w.Body.String())
	}
}

func TestRecoveredLateReceiptColdInterruptedRecoveryKeepsActualEvidence(t *testing.T) {
	var prompts atomic.Int64
	m, old, actual := lateClientFixture(t, "function", func(s *session, f frame) any {
		prompts.Add(1)
		s.events <- agentEvent{Text: "continued interrupted conversation using late receipt"}
		return map[string]any{"stopReason": "end_turn"}
	})
	v := closingCheckpoint(old, nil)
	v.Interrupted = true
	if err := m.disk.put(v, true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	w := recoveryRequest(m, lateClientBody(t, "previous", actual))
	if w.Code != 200 || prompts.Load() != 1 {
		t.Fatalf("interrupted recovery lost late receipt: %d %s", w.Code, w.Body.String())
	}
	for _, current := range m.sessions {
		h, err := inspectToolHistory(current.items)
		if err != nil || len(h.Records) != 3 || h.Records[2].Outcome != "success" || h.Records[2].Provenance != "client_late_result" || !current.responses["previous"] && len(current.responses) < 2 {
			t.Fatal("limited native recovery lost logical history or response lineage", err)
		}
	}
}

func TestRecoveredLateReceiptDoesNotBlockRecoveryOfNextHandoff(t *testing.T) {
	var prompts atomic.Int64
	fn := tool{Type: "function", Name: "save_document", Parameters: map[string]any{"type": "object"}}
	m, _, actual := lateClientFixture(t, "function", func(s *session, f frame) any {
		if prompts.Add(1) == 1 {
			c := &pendingTool{ID: "next-independent-call", Tool: fn, Arguments: map[string]any{"path": "independent.docx"}, Ready: make(chan struct{})}
			s.mu.Lock()
			s.pending[c.ID] = c
			s.mu.Unlock()
			s.events <- agentEvent{Call: c}
		} else {
			s.events <- agentEvent{Text: "next missing operation skipped; original late receipt retained"}
		}
		return map[string]any{"stopReason": "end_turn"}
	})
	body := lateClientBody(t, "previous", actual)
	recoveryOutput(t, identifiedRecoveryRequest(m, body, "late-with-next-tool"))
	if err := m.clearResponseReplays(); err != nil {
		t.Fatal(err)
	}
	w := identifiedRecoveryRequest(m, body, "late-with-next-tool")
	if w.Code != 200 || prompts.Load() != 2 || !strings.Contains(w.Body.String(), "original late receipt retained") {
		t.Fatalf("late receipt blocked existing handoff recovery: %d %s", w.Code, w.Body.String())
	}
	for _, s := range m.sessions {
		h, err := inspectToolHistory(s.items)
		if err != nil || len(h.Records) != 4 || h.Records[2].Provenance != "client_late_result" || h.Records[2].Outcome != "success" || h.Records[3].Outcome != "unknown" {
			t.Fatal("new recovery damaged the original correction", err)
		}
	}
}

func TestRecoveredLateReceiptFailurePreservesFactsAndContinuation(t *testing.T) {
	var prompts atomic.Int64
	m, s, actual := lateClientFixture(t, "function", func(s *session, f frame) any {
		if prompts.Add(1) == 1 {
			return &rpcError{Code: -32000, Message: "injected provider failure"}
		}
		s.events <- agentEvent{Text: "continued after provider failure"}
		return map[string]any{"stopReason": "end_turn"}
	})
	w := recoveryRequest(m, lateClientBody(t, "", actual))
	if w.Code < 400 || prompts.Load() != 1 {
		t.Fatal("injected failure did not occur", w.Body.String())
	}
	var response struct {
		Error struct {
			ResponseID string `json:"response_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	// Use an existing owned response if the transport omitted the new ID.
	previous := response.Error.ResponseID
	if previous == "" {
		for id := range s.responses {
			if strings.HasPrefix(id, "resp_agy_") {
				previous = id
			}
		}
	}
	if previous == "" || m.disk.count.Load() != 1 {
		t.Fatal("failure lost the continuation checkpoint", w.Body.String())
	}
	for _, current := range m.sessions {
		if current.owner == s.owner {
			t.Fatal("failed worker was not removed")
		}
	}
	w = recoveryRequest(m, lateClientBody(t, previous, map[string]any{"role": "user", "content": "continue; the old save already has an actual receipt"}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "continued after provider failure") || prompts.Load() != 2 {
		t.Fatalf("new message could not continue: %d %s", w.Code, w.Body.String())
	}
	for _, current := range m.sessions {
		h, err := inspectToolHistory(current.items)
		if err != nil || len(h.Records) != 3 || h.Records[2].Provenance != "client_late_result" || h.Records[2].Outcome != "success" {
			t.Fatal("provider failure lost the late actual result", err)
		}
	}
}
