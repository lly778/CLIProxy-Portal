package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func originRPCFixture(t *testing.T, m *Manager, s *session) (*process, *json.Encoder, <-chan frame, <-chan nativeEventOrigin) {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	p := &process{cmd: &exec.Cmd{}, in: inWriter, pending: map[string]chan rpcReply{}, done: make(chan struct{}), captureOrigin: m.captureNativeOrigin, finishOrigin: m.finishNativeOrigin, onRequest: m.permissionFrom}
	updates := make(chan nativeEventOrigin, 16)
	p.onUpdate = func(o nativeEventOrigin, raw json.RawMessage) { m.updateFrom(o, raw); updates <- o }
	s.proc, m.proc = p, p
	requests := make(chan frame, 16)
	go p.read(outReader)
	go func() {
		d := json.NewDecoder(inReader)
		for {
			var f frame
			if d.Decode(&f) != nil {
				return
			}
			requests <- f
		}
	}()
	t.Cleanup(func() { p.close(); inReader.Close(); outReader.Close(); outWriter.Close() })
	return p, json.NewEncoder(outWriter), requests, updates
}

func TestNativeReaderClosesPromptBeforeLateNotificationDispatch(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	_, writer, requests, updates := originRPCFixture(t, m, s)
	m.startPrompt(s, []any{map[string]string{"type": "text", "text": "fixture"}})
	var request frame
	select {
	case request = <-requests:
	case <-time.After(time.Second):
		t.Fatal("prompt not written")
	}
	writeUpdate := func(update map[string]any) nativeEventOrigin {
		t.Helper()
		if err := writer.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": s.ID, "update": update}}); err != nil {
			t.Fatal(err)
		}
		select {
		case o := <-updates:
			return o
		case <-time.After(time.Second):
			t.Fatal("notification not processed")
		}
		return nativeEventOrigin{}
	}
	if !writeUpdate(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"text": "real current text"}}).Active {
		t.Fatal("active prompt was not recognized")
	}
	if err := writer.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"stopReason": "end_turn"}}); err != nil {
		t.Fatal(err)
	}
	for _, update := range []map[string]any{
		{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"text": "late text"}},
		{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"text": retryTestWarning}},
		{"sessionUpdate": "tool_call", "toolCallId": "late-official", "name": "search_web", "status": "completed"},
	} {
		if writeUpdate(update).Active {
			t.Fatal("reader kept completed prompt active")
		}
	}
	var got []agentEvent
	for len(got) < 2 {
		select {
		case e := <-s.events:
			got = append(got, e)
		case <-time.After(time.Second):
			t.Fatal("valid text or native completion lost")
		}
	}
	if got[0].Text != "real current text" || got[1].Result == nil || len(s.events) != 0 || len(s.reported) != 0 || s.promptWarning != nil {
		t.Fatal("late notification escaped reader boundary")
	}
}

func TestNativePermissionKeepsReaderEpochAcrossAsyncScheduling(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.running, s.handshake, s.epoch = true, true, 1
	s.reported["permission-call"] = reportedTool{Server: bridgeName, Name: callToolName}
	p, writer, requests, _ := originRPCFixture(t, m, s)
	captured, release := make(chan nativeEventOrigin, 1), make(chan struct{})
	p.onRequest = func(o nativeEventOrigin, method string, raw json.RawMessage) (any, error) {
		captured <- o
		select {
		case <-release:
		case <-t.Context().Done():
			return nil, t.Context().Err()
		}
		return m.permissionFrom(o, method, raw)
	}
	raw := map[string]any{"jsonrpc": "2.0", "id": "native-permission", "method": "session/request_permission", "params": map[string]any{"sessionId": s.ID, "toolCall": map[string]string{"toolCallId": "permission-call"}, "options": []any{map[string]string{"optionId": "yes", "kind": "allow_once"}, map[string]string{"optionId": "no", "kind": "reject_once"}}}}
	if err := writer.Encode(raw); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-captured:
		if o.Epoch != 1 || !o.Active {
			t.Fatal("wrong reader origin")
		}
	case <-time.After(time.Second):
		t.Fatal("permission not received")
	}
	s.mu.Lock()
	s.epoch = 2
	s.mu.Unlock()
	close(release)
	select {
	case reply := <-requests:
		var result struct {
			Outcome struct {
				ID string `json:"optionId"`
			} `json:"outcome"`
		}
		if json.Unmarshal(reply.Result, &result) != nil || result.Outcome.ID != "no" {
			t.Fatal("old permission authorized a later prompt")
		}
	case <-time.After(time.Second):
		t.Fatal("permission reply missing")
	}
}

func TestNativeOriginRejectsReplacedProcessAndSessionObject(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.running, s.handshake, s.epoch = true, true, 1
	raw := json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"old-call","name":"search_web","status":"completed"}}`)
	o := m.captureNativeOrigin(s.proc, raw)
	oldProcess := s.proc
	s.proc = &process{}
	m.updateFrom(o, raw)
	if len(s.events) != 0 || len(s.reported) != 0 || len(s.nativeToolOrder) != 0 {
		t.Fatal("old process mutated new session")
	}
	s.proc = oldProcess
	o = m.captureNativeOrigin(s.proc, raw)
	replacement := &session{ID: s.ID, proc: s.proc, epoch: s.epoch, running: true, events: make(chan agentEvent, 4)}
	m.sessions[s.tenant] = replacement
	m.updateFrom(o, raw)
	if len(replacement.events) != 0 || len(replacement.nativeToolOrder) != 0 {
		t.Fatal("old session object poisoned restored identity")
	}
}

func TestNativeToolIDsRejectOldEpochButKeepIndependentIdenticalCalls(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.running, s.epoch = true, 1
	call := func(id string) {
		retryTestUpdate(m, s, map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "name": "search_web", "rawInput": map[string]any{"query": "same query"}, "status": "completed"})
	}
	call("old-id")
	<-s.events
	s.epoch = 2
	s.reported = map[string]reportedTool{}
	call("old-id")
	if len(s.events) != 0 || len(s.reported) != 0 {
		t.Fatal("old native tool became a new action")
	}
	call("fresh-id-one")
	call("fresh-id-two")
	if len(s.events) != 2 || len(s.reported) != 2 {
		t.Fatal("new equal-argument calls were collapsed")
	}
	for i := 0; i < nativeToolIdentityLimit+10; i++ {
		rememberNativeTool(s, fmt.Sprintf("bounded-%d", i))
	}
	if len(s.nativeToolOrder) != nativeToolIdentityLimit || len(s.nativeToolEpochs) != nativeToolIdentityLimit {
		t.Fatal("native identity retention unbounded")
	}
}

func TestNativeToolIdentitySurvivesEncryptedSnapshotWithoutRawIDs(t *testing.T) {
	_, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.epoch = 1
	rememberNativeTool(s, "private-native-tool-identity")
	v := snapshotSession(s)
	data, _ := json.Marshal(v)
	if strings.Contains(string(data), "private-native-tool-identity") {
		t.Fatal("raw native identity persisted")
	}
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	restored, err := d.loadSession(s.ID, s.owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	restored.epoch = 1
	if nativeToolCurrent(restored, "private-native-tool-identity") || !nativeToolCurrent(restored, "new-independent-id") {
		t.Fatal("cold identity protection lost")
	}
}

func TestNativeCompletionRejectsNewMCPActionBeforeHTTPConsumerRuns(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.secret = "fixture-secret"
	s.running, s.epoch, s.promptCompletedEpoch = true, 3, 3
	s.tools = []tool{{Type: "function", Name: "save", Parameters: map[string]any{"type": "object"}}}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/mcp/"+s.tenant, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"portal_call_tool","arguments":{"name":"save","arguments":{}}}}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+s.secret)
	w := httptest.NewRecorder()
	m.mcp(w, r)
	if len(s.pending) != 0 || !strings.Contains(w.Body.String(), "not allowed") {
		t.Fatal("finished native turn dispatched a new client action")
	}
}

func TestNativeRestoredWaiterIsNotAnActivePrompt(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	_, writer, _, updates := originRPCFixture(t, m, s)
	s.running, s.epoch = true, 0
	if err := writer.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": s.ID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"text": "load replay after waiter bind"}}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-updates:
		if o.Active {
			t.Fatal("restored waiter was declared active")
		}
	case <-time.After(time.Second):
		t.Fatal("notification not processed")
	}
	if len(s.events) != 0 {
		t.Fatal("restored waiter emitted generation output")
	}
}

func TestNativeNotificationPreservesExactToolArgument(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.running, s.epoch = true, 1
	m.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"exact-argument","name":"search_web","rawInput":{"query":"fixture","record":9007199254740993},"status":"completed"}}`))
	e := <-s.events
	n, ok := e.Official.Args["record"].(json.Number)
	if !ok || n.String() != "9007199254740993" {
		t.Fatal("native notification rounded an exact argument")
	}
}

func TestUnnegotiatedMCPOverACPRequestsFailWithoutStoppingReader(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	p, writer, requests, _ := originRPCFixture(t, m, s)
	for i, method := range []string{"mcp/connect", "mcp/message", "mcp/disconnect"} {
		if err := writer.Encode(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": method, "params": map[string]any{"connectionId": "unnegotiated"}}); err != nil {
			t.Fatal(err)
		}
		select {
		case reply := <-requests:
			if reply.Error == nil || reply.Error.Code != -32601 {
				t.Fatal("unnegotiated transport was accepted")
			}
		case <-time.After(time.Second):
			t.Fatal("unsupported transport blocked reader")
		}
	}
	if !p.alive() || len(s.pending) != 0 || len(s.events) != 0 {
		t.Fatal("unsupported ACP MCP method changed live execution")
	}
}
