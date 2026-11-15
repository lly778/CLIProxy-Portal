package agy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func detachedFixture(t *testing.T) (*Manager, *session, []map[string]any) {
	t.Helper()
	m, s := recoveryFixture(t, func(*session, frame) any { return map[string]any{"stopReason": "end_turn"} })
	m.cfg.Secret = strings.Repeat("s", 64)
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.db.Close() })
	m.disk = d
	base := cacheMatchItems()
	base[3]["output"] = strings.Repeat("verified original contents ", 9000)
	fn := tool{Type: "function", Name: "write_document", Parameters: map[string]any{"type": "object"}}
	args := map[string]any{"path": "fixture.docx", "record": json.Number("9007199254740993")}
	c := &pendingTool{ID: "unacknowledged-write", Tool: fn, Arguments: args, Delivered: true, Ready: make(chan struct{})}
	s.tools = []tool{fn}
	s.advertisedTools = append([]tool(nil), s.tools...)
	s.toolHash = digest(s.tools)
	s.officialVersion = officialToolsVersion
	s.items = append(copyItems(base), map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "last response the caller did not receive"}}}, pendingCheckpointItem(c))
	s.history = requestFingerprints(s.items)
	s.pending = map[string]*pendingTool{c.ID: c}
	s.waitAt = time.Now()
	s.running = true
	return m, s, base
}

func detachedRequestItems(base []map[string]any) []map[string]any {
	return append(copyItems(base), map[string]any{"type": "message", "role": "user", "content": "continue after closing the client; verify uncertain work first"}, map[string]any{"type": "message", "role": "developer", "content": "keep this new instruction"})
}

func detachedBody(t *testing.T, items []map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "agy/test", "input": items, "tools": []tool{{Type: "function", Name: "write_document", Parameters: map[string]any{"type": "object"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestDetachedColdResumeKeepsNativeIDAndNextClientHistory(t *testing.T) {
	m, old, base := detachedFixture(t)
	if err := m.disk.put(closingCheckpoint(old, nil), true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	var loads, news, prompts atomic.Int64
	var seen []string
	m.proc = diskTestProcess(t, func(f frame) any {
		switch f.Method {
		case "session/load":
			loads.Add(1)
		case "session/new":
			news.Add(1)
		case "session/prompt":
			prompts.Add(1)
			seen = append(seen, string(f.Params))
			m.mu.Lock()
			for _, s := range m.sessions {
				s.events <- agentEvent{Text: "continued on original native session"}
			}
			m.mu.Unlock()
			return map[string]any{"stopReason": "end_turn"}
		}
		return map[string]any{"sessionId": old.ID, "configOptions": old.options}
	})
	client := detachedRequestItems(base)
	w := recoveryRequest(m, detachedBody(t, client))
	if w.Code != 200 || loads.Load() != 1 || news.Load() != 0 || prompts.Load() != 1 || !strings.Contains(w.Body.String(), "continued on original") {
		t.Fatalf("detached restore failed: load=%d new=%d prompt=%d %d %.1200s", loads.Load(), news.Load(), prompts.Load(), w.Code, w.Body.String())
	}
	if !strings.Contains(seen[0], "UNKNOWN") || !strings.Contains(seen[0], "keep this new instruction") || strings.Contains(seen[0], "verified original contents") || strings.Contains(seen[0], "Completed client tool result") {
		t.Fatal("resume replayed history, lost steering or fabricated completion")
	}
	var response struct {
		Output []map[string]any `json:"output"`
	}
	if unmarshalExact(w.Body.Bytes(), &response) != nil {
		t.Fatal("invalid response")
	}
	var s *session
	for _, candidate := range m.sessions {
		s = candidate
	}
	if s == nil || s.ID != old.ID || len(s.recoveryOverlays) != 1 {
		t.Fatal("native identity or hidden-tail map lost")
	}
	projected, ok := recoveryProjection(s.items, s.recoveryOverlays)
	client = append(client, response.Output...)
	if !ok || !reflect.DeepEqual(requestFingerprints(projected), requestFingerprints(client)) {
		t.Fatal("first recovery cannot accept the client's next history")
	}
	if !uncertainRecoveryAction(s, s.tools[0], map[string]any{"path": "fixture.docx", "record": json.Number("9007199254740993")}) {
		t.Fatal("uncertain write can be automatically repeated")
	}
	client = append(client, map[string]any{"role": "user", "content": "next followup without server-only tail"})
	second := recoveryRequest(m, detachedBody(t, client))
	if second.Code != 200 || news.Load() != 0 || prompts.Load() != 2 || loads.Load() != 1 {
		t.Fatalf("second turn rebuilt or failed: %d %.1000s", second.Code, second.Body.String())
	}
	if strings.Contains(seen[1], "verified original contents") || !strings.Contains(seen[1], "next followup") {
		t.Fatal("second turn replayed history or lost input")
	}
	if unmarshalExact(second.Body.Bytes(), &response) != nil {
		t.Fatal("invalid second response")
	}
	client = append(client, response.Output...)
	if err := m.archiveSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	delete(m.sessions, s.tenant)
	client = append(client, map[string]any{"role": "user", "content": "continue after another cold load"})
	third := recoveryRequest(m, detachedBody(t, client))
	if third.Code != 200 || loads.Load() != 2 || news.Load() != 0 || prompts.Load() != 3 {
		t.Fatalf("cold overlay routing lost: %d %.1000s", third.Code, third.Body.String())
	}
	if len(m.toolRecords) != 0 {
		t.Fatal("normal continuation unexpectedly called the summarizer")
	}
}

func TestDetachedTailIndexRequiresExactPrefixAndUniqueOwner(t *testing.T) {
	m, s, base := detachedFixture(t)
	v := closingCheckpoint(s, nil)
	if err := m.disk.put(v, true); err != nil {
		t.Fatal(err)
	}
	r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
	if err != nil {
		t.Fatal(err)
	}
	if found, err := m.disk.find(s.owner, r, nil); err != nil || found == nil || found.ID != s.ID {
		t.Fatal("indexed detached lookup failed", err)
	}
	actual := r
	actual.Items = append(copyItems(r.Items), map[string]any{"type": "function_call_output", "call_id": "unacknowledged-write", "output": "actual late result"})
	if found, err := m.disk.find(s.owner, actual, nil); err != nil || found == nil {
		t.Fatal("actual late result lost its original call", err)
	} else if _, detached := detachedTailMatch(found, actual); detached {
		t.Fatal("actual result was replaced by unknown")
	}
	for _, mode := range []string{"owner", "model", "system", "old_result", "assistant_suffix", "checkpoint", "missing_common_tool"} {
		t.Run(mode, func(t *testing.T) {
			x := r
			x.Items = copyItems(r.Items)
			owner := s.owner
			switch mode {
			case "owner":
				owner = "other:key"
			case "model":
				x.Model = "agy/other"
			case "system":
				x.Instructions = json.RawMessage(`"changed"`)
			case "old_result":
				x.Items[3]["output"] = "changed contents"
			case "assistant_suffix":
				x.Items = append(x.Items, map[string]any{"role": "assistant", "content": "untrusted continuation"})
			case "checkpoint":
				x.Items = x.Items[:len(x.Items)-1]
				x.Items[len(x.Items)-1]["content"] = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM"
				x.Tools = nil
				x.ToolsProvided = true
			case "missing_common_tool":
				x.Items = x.Items[3:]
			}
			if found, err := m.disk.find(owner, x, nil); err != nil || found != nil {
				t.Fatal("unsafe prefix was accepted", mode, err)
			}
		})
	}
	duplicate := v
	duplicate.ID = "second-eligible-native"
	duplicate.Responses = map[string]bool{"second-response": true}
	if err := m.disk.put(duplicate, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.disk.find(s.owner, r, nil); err == nil {
		t.Fatal("ambiguous old tails were merged")
	}
}

func TestDetachedUnknownIsNotAClientSuccessOrFailure(t *testing.T) {
	m, s, base := detachedFixture(t)
	s.running = false
	r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.prepareDetachedResume(s, &r); err != nil {
		t.Fatal(err)
	}
	c := s.pending["unacknowledged-write"]
	if c.outcome() != "unknown" || !isRecoveryUnknown(c.Output) {
		t.Fatal("missing result was invented")
	}
	blocks, _, err := lateResultPrompt(s)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(blocks)
	if strings.Contains(string(data), "Client tool reported failure") || strings.Contains(string(data), "Completed client tool result") {
		t.Fatal("recovery notice claims an actual client result")
	}
	h, err := inspectToolHistory(r.Items)
	if err != nil || len(h.Open) != 0 {
		t.Fatal("unknown state cannot survive compaction", err)
	}
	last := h.Records[len(h.Records)-1]
	if last.Outcome != "unknown" || last.Provenance != "bridge_recovery_no_client_result" {
		t.Fatal("archive loses uncertainty or provenance")
	}
	before := len(s.pending)
	if call, _ := parkToolCall(s, callToolName, map[string]any{"name": "write_document", "arguments": map[string]any{"path": "fixture.docx", "record": json.Number("9007199254740993")}}); call != nil || len(s.pending) != before {
		t.Fatal("unknown write was repeated")
	}
	if uncertainRecoveryAction(s, s.tools[0], map[string]any{"path": "another.docx"}) {
		t.Fatal("independent operation blocked")
	}
}

func TestDetachedWarmResumeArchivesBeforeLoadingSameID(t *testing.T) {
	m, old, base := detachedFixture(t)
	var methods []string
	m.proc = diskTestProcess(t, func(f frame) any {
		methods = append(methods, f.Method)
		if f.Method == "session/prompt" {
			m.mu.Lock()
			for _, s := range m.sessions {
				s.events <- agentEvent{Text: "warm waiter safely resumed"}
			}
			m.mu.Unlock()
			return map[string]any{"stopReason": "end_turn"}
		}
		return map[string]any{"sessionId": old.ID, "configOptions": old.options}
	})
	old.proc = m.proc
	w := recoveryRequest(m, detachedBody(t, detachedRequestItems(base)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "warm waiter safely resumed") {
		t.Fatalf("warm resume failed: %d %.1000s", w.Code, w.Body.String())
	}
	joined := strings.Join(methods, ",")
	if !strings.Contains(joined, "session/close") || !strings.Contains(joined, "session/load") || strings.Contains(joined, "session/new") {
		t.Fatal("warm recovery changed native ID", joined)
	}
}

func TestDetachedIndexUpgradesLegacyCacheWithoutChangingHistory(t *testing.T) {
	m, s, _ := detachedFixture(t)
	v := closingCheckpoint(s, nil)
	if err := m.disk.put(v, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.disk.db.Exec(`DELETE FROM session_tail_routes; UPDATE cache_meta SET value=? WHERE id=2`, []byte("exact-numbers-remote-mcp-identities-v4")); err != nil {
		t.Fatal(err)
	}
	if err := m.disk.upgradeHistoryIndex(); err != nil {
		t.Fatal(err)
	}
	var routes int
	if err := m.disk.db.QueryRow(`SELECT COUNT(*) FROM session_tail_routes WHERE session_id=?`, s.ID).Scan(&routes); err != nil || routes != 1 {
		t.Fatal("legacy detached route not indexed", err)
	}
	cold, err := m.disk.loadSession(s.ID, s.owner, 0)
	if err != nil || len(cold.items) != len(v.Items) {
		t.Fatal("index upgrade changed old history", err)
	}
	if !reflect.DeepEqual(requestFingerprints(cold.items), requestFingerprints(v.Items)) {
		t.Fatal("index upgrade rewrote source items")
	}
}

func TestDetachedBusyAndActiveDeliveryDoNotCancelWaiter(t *testing.T) {
	for _, mode := range []string{"request_busy", "active_delivery", "invalid_policy", "oversized_new_input", "queued_followup"} {
		t.Run(mode, func(t *testing.T) {
			m, s, base := detachedFixture(t)
			r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "request_busy":
				s.requestMu.Lock()
				defer s.requestMu.Unlock()
			case "active_delivery":
				s.pending["unacknowledged-write"].traceRequest = "old-request"
				m.replays = map[string]*responseReplay{"key": {originRequest: "old-request", readers: 1, done: true}}
			case "invalid_policy":
				r.ToolChoice = json.RawMessage(`{"type":"function","name":"not-declared"}`)
			case "oversized_new_input":
				r.Items[len(r.Items)-2]["content"] = strings.Repeat("x", MaxText+1)
			case "queued_followup":
				s.followups = []*queuedFollowup{{}}
			}
			before := requestFingerprints(s.items)
			if _, err = m.coldDetachedSession(t.Context(), s, &r); err == nil {
				t.Fatal("unsafe takeover accepted", mode)
			}
			if s.life.Err() != nil || s.loading || !reflect.DeepEqual(before, requestFingerprints(s.items)) {
				t.Fatal("failed preflight cancelled or modified original waiter")
			}
			select {
			case <-s.pending["unacknowledged-write"].Ready:
				t.Fatal("failed validation released old tool")
			default:
			}
		})
	}
}

func TestDetachedFailedNativeLoadNeverCreatesReplacementOrEditsSnapshot(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			m, old, base := detachedFixture(t)
			v := closingCheckpoint(old, nil)
			if err := m.disk.put(v, true); err != nil {
				t.Fatal(err)
			}
			m.sessions = map[string]*session{}
			var news atomic.Int64
			m.proc = diskTestProcess(t, func(f frame) any {
				if f.Method == "session/new" {
					news.Add(1)
				}
				if f.Method == "session/load" {
					if missing {
						return &rpcError{-32602, "session not found"}
					}
					return &rpcError{-32603, "unavailable fixture"}
				}
				return map[string]any{}
			})
			w := recoveryRequest(m, detachedBody(t, detachedRequestItems(base)))
			if w.Code == 200 || news.Load() != 0 {
				t.Fatal("failed same-ID load substituted a new session")
			}
			cold, err := m.disk.loadSession(old.ID, old.owner, 0)
			if err != nil || cold == nil || len(cold.recoveryOverlays) != 0 || !reflect.DeepEqual(requestFingerprints(cold.items), requestFingerprints(v.Items)) {
				t.Fatal("failed load changed recovery snapshot", err)
			}
		})
	}
}

func TestDetachedArchiveFailureLeavesNativeWaitAlive(t *testing.T) {
	m, s, base := detachedFixture(t)
	r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.disk.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = m.coldDetachedSession(t.Context(), s, &r); err == nil {
		t.Fatal("failed archive was accepted")
	}
	if s.life.Err() != nil || s.loading || m.sessions[s.tenant] != s {
		t.Fatal("failed archive destroyed live wait")
	}
}

func TestDetachedProjectionRejectsTamperAndPreservesCompactionFacts(t *testing.T) {
	m, s, base := detachedFixture(t)
	s.running = false
	r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.prepareDetachedResume(s, &r); err != nil {
		t.Fatal(err)
	}
	items := copyItems(r.Items)
	overlays := cloneRecoveryOverlays(s.recoveryOverlays)
	items[overlays[0].At]["summary"] = []any{map[string]any{"type": "summary_text", "text": "rewritten"}}
	if _, ok := recoveryProjection(items, overlays); ok {
		t.Fatal("rewritten hidden tail accepted")
	}
	h, err := inspectToolHistory(r.Items)
	if err != nil {
		t.Fatal(err)
	}
	note, archive, err := m.prepareToolRecordNote(t.Context(), s.owner, s.model, r.Items, h.Records, digest(r.Items))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.saveToolRecords(t.Context(), *archive); err != nil {
		t.Fatal(err)
	}
	refs, err := m.previousToolRecords(s.owner, s.model, []map[string]any{{"content": note}})
	if err != nil || len(refs) != 1 {
		t.Fatal("unknown record lost protected index", err)
	}
	read := m.readToolRecord(t.Context(), s.owner, s.model, map[string]any{"reference": refs[0], "call_id": "unacknowledged-write"})
	data, _ := json.Marshal(read)
	if !strings.Contains(string(data), "UNKNOWN") || strings.Contains(string(data), "Client tool reported failure") {
		t.Fatal("archive invented a result for unknown action")
	}
}

func TestDetached686ItemPrefixMatchesTheObserved688ItemShape(t *testing.T) {
	m, s, _ := detachedFixture(t)
	var base []map[string]any
	for i := 0; i < 11; i++ {
		base = append(base, map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("fixture source message %d", i)})
	}
	for i := 0; i < 262; i++ {
		if i < 151 {
			base = append(base, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": fmt.Sprintf("fixture reasoning %d", i)}}})
		}
		id := fmt.Sprintf("complete-call-%d", i)
		base = append(base, map[string]any{"type": "function_call", "name": "read", "call_id": id, "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": id, "output": "actual prior result"})
	}
	if len(base) != 686 {
		t.Fatal("invalid fixture shape")
	}
	c := s.pending["unacknowledged-write"]
	s.items = append(copyItems(base), map[string]any{"type": "reasoning", "summary": []any{}}, pendingCheckpointItem(c))
	s.history = requestFingerprints(s.items)
	r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 688 {
		t.Fatal("incoming fixture shape differs")
	}
	if cut, ok := detachedTailMatch(s, r); !ok || cut != 686 {
		t.Fatal("same-length diverging tail was not matched")
	}
	if err = m.disk.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	found, err := m.disk.find(s.owner, r, nil)
	if err != nil || found == nil || found.ID != s.ID {
		t.Fatal("indexed lookup missed observed prefix shape", err)
	}
}

func TestDetachedMCPRetryExplainsUnknownAndDoesNotDispatch(t *testing.T) {
	m, s, base := detachedFixture(t)
	s.running = false
	r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.prepareDetachedResume(s, &r); err != nil {
		t.Fatal(err)
	}
	s.secret = "fixture-secret"
	s.running = true
	before := len(s.pending)
	result := localMCPCall(t, m, s, "tools/call", map[string]any{"name": callToolName, "arguments": map[string]any{"name": "write_document", "arguments": map[string]any{"path": "fixture.docx", "record": json.Number("9007199254740993")}}})
	data, _ := json.Marshal(result)
	if !toolResultFailed(result) || !strings.Contains(string(data), "UNKNOWN") || len(s.pending) != before {
		t.Fatal("MCP retry dispatched or hid unknown outcome")
	}
}

func TestDetachedCodexCheckpointKeepsHiddenUnknownEvidence(t *testing.T) {
	m, s, base := detachedFixture(t)
	s.running = false
	r, err := parseRequest([]byte(detachedBody(t, detachedRequestItems(base))))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.prepareDetachedResume(s, &r); err != nil {
		t.Fatal(err)
	}
	s.items = r.Items
	s.history = requestFingerprints(s.items)
	s.pending = map[string]*pendingTool{}
	projected, ok := recoveryProjection(s.items, s.recoveryOverlays)
	if !ok {
		t.Fatal("invalid projection")
	}
	var input []map[string]any
	for _, item := range projected {
		if item["type"] != "reasoning" {
			input = append(input, item)
		}
	}
	input = append(input, map[string]any{"role": "user", "content": "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM"})
	checkpoint := request{Model: s.model, Previous: "previous", Items: input}
	if _, err = m.prepareCodexCompaction(s.owner, &checkpoint); err != nil {
		t.Fatal("checkpoint lost recovered lineage", err)
	}
	h, err := inspectToolHistory(checkpoint.Items[:len(checkpoint.Items)-1])
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Records) != 3 || h.Records[2].Outcome != "unknown" || h.Records[2].Provenance != "bridge_recovery_no_client_result" {
		t.Fatal("Codex checkpoint dropped hidden unknown evidence")
	}
}
