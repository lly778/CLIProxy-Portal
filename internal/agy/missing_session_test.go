package agy

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMissingNativeSessionRecoveryUsesOneLoadAndOneNewWithFullEvidence(t *testing.T) {
	for _, createFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "create_failed"}[createFails], func(t *testing.T) {
			for _, nativeCheck := range []bool{false, true} {
				t.Run(map[bool]string{false: "rpc_missing", true: "native_record_missing"}[nativeCheck], func(t *testing.T) {
					m, original := recoveryFixture(t, func(*session, frame) any { return nil })
					d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
					if err != nil {
						t.Fatal(err)
					}
					defer d.db.Close()
					m.disk = d
					original.items = evidenceTranscript()[:3]
					original.history = requestFingerprints(original.items)
					original.officialVersion = officialToolsVersion
					if err = d.put(snapshotSession(original), true); err != nil {
						t.Fatal(err)
					}
					m.sessions = map[string]*session{}
					var loads, news, prompts atomic.Int64
					var actualPrompt string
					m.proc = diskTestProcess(t, func(f frame) any {
						switch f.Method {
						case "session/load":
							loads.Add(1)
							return &rpcError{Code: -32602, Message: "Session not found: native"}
						case "session/new":
							news.Add(1)
							if createFails {
								return &rpcError{Code: -32603, Message: "unknown service error"}
							}
							return map[string]any{"sessionId": "replacement", "configOptions": original.options}
						case "session/prompt":
							prompts.Add(1)
							actualPrompt = string(f.Params)
							m.mu.Lock()
							for _, s := range m.sessions {
								s.events <- agentEvent{Text: "continued with original evidence"}
							}
							m.mu.Unlock()
							return map[string]any{"stopReason": "end_turn"}
						}
						return map[string]any{"configOptions": original.options}
					})
					if nativeCheck {
						m.proc.evidence = newRuntimeEvidence(verifiedAGY153, "agy", "1.5.3", 1, nil)
					}
					w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"continue without saving again"}`)
					wantLoads := int64(1)
					if nativeCheck {
						wantLoads = 0
					}
					if loads.Load() != wantLoads || news.Load() != 1 {
						t.Fatalf("recovery retried unexpectedly: load=%d new=%d", loads.Load(), news.Load())
					}
					if createFails {
						if prompts.Load() != 0 {
							t.Fatal("failed creation still prompted")
						}
						if s, e := d.find("owner", request{Previous: "previous"}, nil); e != nil || s == nil {
							t.Fatal("failed replacement destroyed saved record", e)
						}
						return
					}
					if w.Code != 200 || prompts.Load() != 1 || !strings.Contains(w.Body.String(), "continued with original evidence") {
						t.Fatalf("recovery failed: %d %.1200s", w.Code, w.Body.String())
					}
					for _, want := range []string{"save-once", "retained-actual-save-result", "9007199254740993", "continue without saving again"} {
						if !strings.Contains(actualPrompt, want) {
							t.Fatal("recovery omitted evidence", want)
						}
					}
					if s, e := d.find("owner", request{Previous: "previous"}, nil); e != nil || s != nil {
						t.Fatal("old disk lineage remained usable", e)
					}
				})
			}
		})
	}
}

func TestMissingSessionClassificationAndUnsafeStatesNeverFallback(t *testing.T) {
	for _, message := range []string{"load failed", "timeout: session not found", "session not found while contacting upstream", "unknown session API error"} {
		if explicitMissingSession(&rpcError{-32603, message}) {
			t.Fatal("uncertain failure classified as confirmed missing", message)
		}
	}
	if explicitMissingSession(&rpcError{-32601, "session not found"}) {
		t.Fatal("unsupported method treated as missing session")
	}
	_, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.items = evidenceTranscript()[:3]
	s.history, s.persisted = requestFingerprints(s.items), true
	r := request{Model: s.model, Items: append(copyItems(s.items), map[string]any{"role": "user", "content": "continue"})}
	if !canRebuildMissingSession(s, r) {
		t.Fatal("complete confirmed state rejected")
	}
	for _, mode := range []string{"pending", "running", "interrupted", "waiting", "result", "open", "checkpoint"} {
		v := *rCopyForMissingTest(r)
		s.running, s.interrupted, s.waitAt = false, false, time.Time{}
		s.pending = map[string]*pendingTool{}
		switch mode {
		case "pending":
			s.pending["save-once"] = &pendingTool{}
		case "running":
			s.running = true
		case "interrupted":
			s.interrupted = true
		case "waiting":
			s.waitAt = time.Now()
		case "result":
			v.Items = append(v.Items, map[string]any{"type": "function_call_output", "call_id": "save-once", "output": "again"})
		case "open":
			v.Items = append(v.Items, map[string]any{"type": "function_call", "call_id": "new", "name": "write", "arguments": "{}"})
		case "checkpoint":
			v.Items[len(v.Items)-1]["content"] = codexCompactFixture
		}
		if canRebuildMissingSession(s, v) {
			t.Fatal("unsafe fallback allowed", mode)
		}
	}
}

func rCopyForMissingTest(r request) *request { v := r; v.Items = copyItems(r.Items); return &v }

func TestMissingClassificationIsRestrictedToNativeLoadRPC(t *testing.T) {
	p := diskTestProcess(t, func(frame) any { return &rpcError{-32602, "session not found"} })
	for _, method := range []string{"session/load", "session/new", "session/prompt"} {
		var out json.RawMessage
		err := p.call(t.Context(), method, map[string]any{}, &out)
		if isMissingSession(err) != (method == "session/load") {
			t.Fatal("missing classification escaped load", method, err)
		}
	}
}
