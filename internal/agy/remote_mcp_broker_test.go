package agy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type remoteRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f remoteRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoteBrokerWaitsForActualResultAfterEarlyNativeCompletion(t *testing.T) {
	var m *Manager
	var prompts atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	m, s := recoveryFixture(t, func(s *session, _ frame) any {
		if prompts.Add(1) == 1 {
			go invokeRemoteNative(t, m, s, 73)
			<-started
			return map[string]any{"stopReason": "end_turn"}
		}
		s.events <- agentEvent{Text: "continued using actual remote completion"}
		return map[string]any{"stopReason": "end_turn"}
	})
	s.secret = "fixture-secret"
	transport, calls := remoteBrokerService(t)
	m.remoteTransport = remoteRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil {
			body, _ := r.GetBody()
			data, _ := io.ReadAll(body)
			body.Close()
			var f map[string]any
			json.Unmarshal(data, &f)
			if f["method"] == "tools/call" {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
		}
		return transport.RoundTrip(r)
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"remote operation","tools":`+remoteBrokerTools("never")+`}`)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("remote operation not started")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("HTTP response ended before actual remote result")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("remote result did not resume generation")
	}
	if w.Code != 200 || calls.Load() != 1 || prompts.Load() != 2 || !strings.Contains(w.Body.String(), "continued using actual remote completion") || !strings.Contains(w.Body.String(), `"type":"mcp_call"`) {
		t.Fatalf("early completion lost remote result: prompts=%d calls=%d %d %.1800s", prompts.Load(), calls.Load(), w.Code, w.Body.String())
	}
}

func remoteBrokerService(t *testing.T) (http.RoundTripper, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f map[string]any
		if json.NewDecoder(r.Body).Decode(&f) != nil {
			t.Error("bad fixture request")
		}
		result := map[string]any{}
		switch f["method"] {
		case "initialize":
			result = map[string]any{"protocolVersion": remoteMCPProtocol, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "write", "description": "Write through the explicitly provided remote service", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"record": map[string]any{"const": json.Number("9007199254740993")}}, "required": []string{"record"}}}}}
		case "tools/call":
			calls.Add(1)
			result = map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": "remote saved once"}}, "structuredContent": map[string]any{"record": json.Number("9007199254740993")}}
		default:
			t.Error("unexpected fixture method", f["method"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": f["id"], "result": result})
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	return remoteTestTransport{target, http.DefaultTransport}, calls
}

func remoteBrokerTools(approval string) string {
	return fmt.Sprintf(`[{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.test/mcp","authorization":"fixture-account-token","allowed_tools":["write"],"require_approval":%q}]`, approval)
}

func invokeRemoteNative(t *testing.T, m *Manager, s *session, id int) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"%s","arguments":{"name":"docs.write","arguments":{"record":9007199254740993}}}}`, id, callToolName)
	r := httptest.NewRequest("POST", "/mcp/"+s.tenant, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.secret)
	w := httptest.NewRecorder()
	m.mcp(w, r)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			var reply map[string]any
			if unmarshalExact([]byte(strings.TrimPrefix(line, "data: ")), &reply) == nil {
				if result, ok := reply["result"].(map[string]any); ok {
					return result
				}
			}
		}
	}
	t.Errorf("native remote MCP result unavailable: %d %.1000s", w.Code, w.Body.String())
	return nil
}

func TestRemoteMCPBrokerNeverApprovalExecutesServerOnlyAndReusesNativeRPC(t *testing.T) {
	var m *Manager
	m, s := recoveryFixture(t, func(s *session, _ frame) any {
		first := invokeRemoteNative(t, m, s, 71)
		second := invokeRemoteNative(t, m, s, 71)
		if digest(first) != digest(second) || toolResultFailed(first) {
			t.Error("same native RPC did not reuse actual remote result")
		}
		if third := invokeRemoteNative(t, m, s, 74); toolResultFailed(third) {
			t.Error("independent native RPC was not executed")
		}
		s.events <- agentEvent{Text: "used actual remote result"}
		return map[string]any{"stopReason": "end_turn"}
	})
	s.secret = "fixture-secret"
	transport, calls := remoteBrokerService(t)
	m.remoteTransport = transport
	w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"write remotely","tools":`+remoteBrokerTools("never")+`}`)
	if w.Code != 200 || calls.Load() != 2 || !strings.Contains(w.Body.String(), `"type":"mcp_call"`) || strings.Contains(w.Body.String(), `"type":"function_call"`) || strings.Contains(w.Body.String(), `"type":"mcp_approval_request"`) || strings.Contains(w.Body.String(), "fixture-account-token") {
		t.Fatalf("remote execution lifecycle failed: calls=%d %d %.2000s", calls.Load(), w.Code, w.Body.String())
	}
	if len(s.remoteConnections) != 0 {
		t.Fatal("request retained authorization")
	}
}

func TestRemoteMCPBrokerApprovalRoundTripNeverTreatsApprovalAsResult(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprint(approve), func(t *testing.T) {
			var m *Manager
			m, s := recoveryFixture(t, func(s *session, _ frame) any {
				result := invokeRemoteNative(t, m, s, 72)
				if toolResultFailed(result) == approve {
					t.Error("remote native result did not reflect actual operation/denial")
				}
				s.events <- agentEvent{Text: "finished after actual remote result or denial"}
				return map[string]any{"stopReason": "end_turn"}
			})
			s.secret = "fixture-secret"
			transport, calls := remoteBrokerService(t)
			m.remoteTransport = transport
			first := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"write remotely","tools":`+remoteBrokerTools("always")+`}`)
			var response struct {
				ID     string           `json:"id"`
				Output []map[string]any `json:"output"`
			}
			if unmarshalExact(first.Body.Bytes(), &response) != nil || len(response.Output) != 2 || response.Output[1]["type"] != "mcp_approval_request" || calls.Load() != 0 {
				t.Fatalf("approval executed early or wire invalid: %d %.1600s", first.Code, first.Body.String())
			}
			id := stringField(response.Output[1], "id")
			s.mu.Lock()
			call := s.pending[id]
			ready := false
			select {
			case <-call.Ready:
				ready = true
			default:
			}
			s.mu.Unlock()
			if ready {
				t.Fatal("unapproved operation already has a result")
			}
			secondBody := fmt.Sprintf(`{"model":"agy/test","previous_response_id":%q,"input":[{"type":"mcp_approval_response","approval_request_id":%q,"approve":%t}],"tools":%s}`, response.ID, id, approve, remoteBrokerTools("always"))
			second := recoveryRequest(m, secondBody)
			want := int64(0)
			if approve {
				want = 1
			}
			if second.Code != 200 || calls.Load() != want || !strings.Contains(second.Body.String(), `"type":"mcp_call"`) || !strings.Contains(second.Body.String(), "finished after actual remote result") {
				t.Fatalf("approval continuation failed: calls=%d %d %.1800s", calls.Load(), second.Code, second.Body.String())
			}
			select {
			case <-call.Ready:
			case <-time.After(time.Second):
				t.Fatal("actual result not released")
			}
		})
	}
}
