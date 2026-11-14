package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteMalformedApprovalNeverWakesExecution(t *testing.T) {
	for _, decision := range []any{nil, "true", 1, map[string]any{"approve": true}} {
		_, s, c, r := remotePendingFixture(t)
		item := map[string]any{"type": "mcp_approval_response", "approval_request_id": c.ID, "approve": decision}
		if err := deliverToolsForRequest(s, []map[string]any{item}, &r); err == nil {
			t.Fatal("ambiguous approval accepted")
		}
		select {
		case <-c.remote.approvalReady:
			t.Fatal("invalid approval woke execution")
		default:
		}
	}
}

func TestRemoteCheckpointPreservesNotExecutedReasonAndCapacity(t *testing.T) {
	m, s, c, _ := remotePendingFixture(t)
	s.remoteRetainedBytes = MaxBody
	c.remote.Approved = true
	close(c.remote.approvalReady)
	m.startRemoteJobs(s)
	select {
	case <-c.Ready:
	case <-time.After(time.Second):
		t.Fatal("capacity refusal missing")
	}
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	v := closingCheckpoint(s, nil)
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	restored, err := d.loadSession(s.ID, s.owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	if restored.remoteRetainedBytes < MaxBody || restored.pending[c.ID].ResultItem["error"] != "remote_capacity" {
		t.Fatal("restart reset capacity or execution refusal reason")
	}
}

func TestRemoteCodexCheckpointSealsResultPublishedDuringSummary(t *testing.T) {
	m, s, c, _ := remotePendingFixture(t)
	approval := map[string]any{"type": "mcp_approval_response", "approval_request_id": c.ID, "approve": true}
	source := append(copyItems(s.items), approval)
	r := request{owner: s.owner, Model: s.model, Items: append(source, map[string]any{"role": "user", "content": "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM"})}
	result := remoteResultItem(c, map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": "actual completion during checkpoint"}}}, "")
	out := newResponse(httptest.NewRecorder(), false, s.model)
	out.textValue = "summary omitted tool detail"
	out.output = append(out.output, result)
	if err := m.finishCodexCheckpoint(t.Context(), r, out); err != nil {
		t.Fatal(err)
	}
	refs, err := m.previousToolRecords(s.owner, s.model, []map[string]any{{"content": out.textValue}})
	if err != nil || len(refs) != 1 {
		t.Fatal("missing result archive", err)
	}
	read := m.readToolRecord(t.Context(), s.owner, s.model, map[string]any{"reference": refs[0], "call_id": c.remote.CallID})
	data, _ := json.Marshal(read)
	if !strings.Contains(string(data), "actual completion during checkpoint") {
		t.Fatal("summary lost actual evidence")
	}
	open := copyItems([]map[string]any{result})
	open[0]["status"] = "in_progress"
	delete(open[0], "output")
	h, err := inspectToolHistory(open)
	if err != nil || len(h.Open) != 1 {
		t.Fatal("in-progress remote record became completed", err)
	}
}

func TestRemoteColdContinuationUsesSavedUnknownResultWithoutNewExecution(t *testing.T) {
	m, s, c, _ := remotePendingFixture(t)
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	c.remote.Approved = true
	c.remote.ApprovalHash = digest(true)
	c.remote.ApprovalItem = map[string]any{"type": "mcp_approval_response", "approval_request_id": c.ID, "approve": true}
	close(c.remote.approvalReady)
	c.remote.Attempted = true
	v := closingCheckpoint(s, nil)
	v.OfficialToolsVersion = officialToolsVersion
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	var prompt string
	m.proc = diskTestProcess(t, func(f frame) any {
		if f.Method == "session/prompt" {
			prompt = string(f.Params)
			m.mu.Lock()
			for _, active := range m.sessions {
				active.events <- agentEvent{Text: "continued from saved uncertainty"}
			}
			m.mu.Unlock()
			return map[string]any{"stopReason": "end_turn"}
		}
		return map[string]any{"sessionId": s.ID, "configOptions": s.options}
	})
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"continue after restart"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "continued from saved uncertainty") || !strings.Contains(prompt, "outcome is unknown") || !strings.Contains(prompt, "continue after restart") {
		t.Fatalf("cold remote continuation lost uncertainty or steering: %d %.1800s", w.Code, w.Body.String())
	}
}

func TestRemoteApprovalColdContinuationExecutesOnlyAfterFreshAuthorization(t *testing.T) {
	m, s, c, _ := remotePendingFixture(t)
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	v := closingCheckpoint(s, nil)
	v.OfficialToolsVersion = officialToolsVersion
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	transport, calls := remoteBrokerService(t)
	m.remoteTransport = transport
	var prompt string
	m.proc = diskTestProcess(t, func(f frame) any {
		if f.Method == "session/prompt" {
			prompt = string(f.Params)
			m.mu.Lock()
			for _, active := range m.sessions {
				active.events <- agentEvent{Text: "used actual cold remote result"}
			}
			m.mu.Unlock()
			return map[string]any{"stopReason": "end_turn"}
		}
		return map[string]any{"sessionId": s.ID, "configOptions": s.options}
	})
	body := fmt.Sprintf(`{"model":"agy/test","previous_response_id":"previous","input":[{"type":"mcp_approval_response","approval_request_id":%q,"approve":true},{"role":"user","content":"keep this cold continuation instruction"}],"tools":%s}`, c.ID, remoteBrokerTools("always"))
	w := recoveryRequest(m, body)
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "used actual cold remote result") || !strings.Contains(prompt, "remote saved once") || !strings.Contains(prompt, "keep this cold continuation instruction") {
		t.Fatalf("cold approval did not preserve real result and instruction: calls=%d %d %.2000s", calls.Load(), w.Code, w.Body.String())
	}
}

func remotePendingFixture(t *testing.T) (*Manager, *session, *pendingTool, request) {
	t.Helper()
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	transport, _ := remoteBrokerService(t)
	m.remoteTransport = transport
	m.cfg.Secret = strings.Repeat("s", 64)
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"remote operation","tools":` + remoteBrokerTools("always") + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.prepareRemoteTools(t.Context(), s.owner, &r); err != nil {
		t.Fatal(err)
	}
	s.tools = r.Tools
	s.toolHash = digest(r.Tools)
	s.advertisedTools = append([]tool{}, r.Tools...)
	s.trace = &requestTrace{m: m, owner: s.owner, model: s.model, request: "remote-fixture", session: s.ID}
	commitRemoteConnections(s, &r)
	c, fresh := parkToolCall(s, callToolName, map[string]any{"name": "docs.write", "arguments": map[string]any{"record": json.Number("9007199254740993")}})
	if !fresh || c.remote == nil {
		t.Fatal("remote pending fixture failed")
	}
	c.Delivered = true
	s.items = []map[string]any{{"role": "user", "content": "remote operation"}, approvalRequestItem(c)}
	s.history = requestFingerprints(s.items)
	return m, s, c, r
}

func TestRemoteApprovalCheckpointPersistsIdentityWithoutAuthorization(t *testing.T) {
	m, s, c, _ := remotePendingFixture(t)
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	v := closingCheckpoint(s, nil)
	encoded, _ := json.Marshal(v)
	if strings.Contains(string(encoded), "fixture-account-token") || len(v.WaitingTools) != 1 || v.WaitingTools[0].Remote == nil {
		t.Fatal("remote checkpoint lost identity or retained bearer")
	}
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	restored, err := d.loadSession(s.ID, s.owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	recovered := restored.pending[c.ID]
	if recovered == nil || recovered.remote == nil || recovered.Tool.remoteBinding == nil || restored.tools[0].remoteBinding == nil || recovered.remote.Binding != c.remote.Binding {
		t.Fatal("remote executor or approval identity lost")
	}
	select {
	case <-recovered.Ready:
		t.Fatal("approval wait became an execution result")
	default:
	}
	select {
	case <-recovered.remote.approvalReady:
		t.Fatal("approval was invented")
	default:
	}
	history, err := inspectToolHistory(v.Items)
	if err != nil || len(history.Open) != 1 {
		t.Fatal("pending approval cannot be protected by compaction", err)
	}
}

func TestRemoteAttemptSnapshotAndJournalPreventAutomaticReexecution(t *testing.T) {
	m, s, c, r := remotePendingFixture(t)
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	if err = m.claimRemoteAttempt(s, c); err != nil {
		t.Fatal(err)
	}
	if err = m.claimRemoteAttempt(s, c); err == nil {
		t.Fatal("same durable attempt claimed twice")
	}
	c.remote.Approved = true
	c.remote.ApprovalHash = digest(true)
	c.remote.ApprovalItem = map[string]any{"type": "mcp_approval_response", "approval_request_id": c.ID, "approve": true}
	close(c.remote.approvalReady)
	c.remote.Attempted = true
	v := closingCheckpoint(s, nil)
	if len(v.WaitingTools) != 1 || !v.WaitingTools[0].Ready || v.WaitingTools[0].Remote.Outcome != "unknown" || !toolResultFailed(v.WaitingTools[0].Output) {
		t.Fatal("in-flight remote execution was not preserved as unknown")
	}
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	restored, err := d.loadSession(s.ID, s.owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	restored.remoteConnections = r.remoteConnections
	recovered := restored.pending[c.ID]
	if recovered.remote.Outcome != "unknown" || recovered.ResultItem["error"] != "execution_unknown" {
		t.Fatal("unknown remote outcome became completed success")
	}
	m.startRemoteJobs(restored)
	if recovered.remote.started {
		t.Fatal("restored unknown execution was automatically restarted")
	}
	h, err := inspectToolHistory(v.Items)
	if err != nil || len(h.Open) != 0 {
		t.Fatal("unknown actual result lost its closed identity", err)
	}
	for _, record := range h.Records {
		if record.Kind == "mcp_call" && record.Outcome != "unknown" {
			t.Fatal("execution uncertainty erased")
		}
	}
}

func TestRemoteApprovalRejectsCredentialScopeAndDefinitionChangesBeforeWake(t *testing.T) {
	for _, mode := range []string{"credential", "scope", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			m, s, c, _ := remotePendingFixture(t)
			tools := remoteBrokerTools("always")
			if mode == "credential" {
				tools = strings.Replace(tools, "fixture-account-token", "replacement-account-token", 1)
			}
			if mode == "scope" {
				tools = strings.Replace(tools, `["write"]`, `[]`, 1)
			}
			r, err := parseRequest([]byte(`{"model":"agy/test","input":"continue","tools":` + tools + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if mode != "omitted" {
				if err = m.prepareRemoteTools(t.Context(), s.owner, &r); err != nil {
					t.Fatal(err)
				}
			}
			item := map[string]any{"type": "mcp_approval_response", "approval_request_id": c.ID, "approve": true}
			if err = deliverToolsForRequest(s, []map[string]any{item}, &r); err == nil {
				t.Fatal("changed remote authority executed old approval", mode)
			}
			select {
			case <-c.remote.approvalReady:
				t.Fatal("invalid approval woke remote operation")
			default:
			}
			if c.remote.Attempted {
				t.Fatal("invalid approval started dispatch")
			}
			if _, err = m.rebuildSession(t.Context(), s, r); err == nil {
				t.Fatal("rebuild discarded unresolved remote operation")
			}
			if s.life.Err() != nil {
				t.Fatal("rejected rebuild cancelled original approval")
			}
		})
	}
}

func TestRemoteCompactionKeepsActualResultsSeparateFromApproval(t *testing.T) {
	m, s, c, _ := remotePendingFixture(t)
	c.remote.Approved = true
	c.remote.ApprovalItem = map[string]any{"type": "mcp_approval_response", "approval_request_id": c.ID, "approve": true}
	result := map[string]any{"isError": false, "content": []any{map[string]any{"type": "image", "mimeType": "image/png", "data": "YQ=="}}, "structuredContent": map[string]any{"record": json.Number("9007199254740993")}}
	actual := remoteResultItem(c, result, "")
	items := append(copyItems(s.items), c.remote.ApprovalItem, actual, map[string]any{"role": "assistant", "content": strings.Repeat("x", MaxText+1)}, map[string]any{"role": "user", "content": "continue without repeating"})
	m.summarizeTest = func(context.Context, string, []any, func()) (string, error) {
		return "summary omitted remote detail", nil
	}
	compacted, err := m.compactWithProof(t.Context(), request{owner: s.owner, Model: s.model, Items: items}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if compacted.Proof.ToolRecords != 2 || compacted.Proof.Images != 1 {
		t.Fatal("approval/result or image coverage lost", compacted.Proof)
	}
	refs, err := m.previousToolRecords(s.owner, s.model, compacted.Items)
	if err != nil || len(refs) != 1 {
		t.Fatal("missing remote fact reference", err)
	}
	read := m.readToolRecord(t.Context(), s.owner, s.model, map[string]any{"reference": refs[0], "call_id": c.remote.CallID})
	encoded, _ := json.Marshal(read)
	if !strings.Contains(string(encoded), "9007199254740993") || !strings.Contains(string(encoded), "image/png") {
		t.Fatal("actual remote result not recoverable")
	}
	other := copyItems([]map[string]any{actual})[0]
	other["id"] = "different-remote-operation"
	if itemFingerprint(other) == itemFingerprint(actual) {
		t.Fatal("different remote operation identity collapsed")
	}
	if len(m.toolRecords) != 1 {
		t.Fatal("unexpected remote fact archive count")
	}
}
