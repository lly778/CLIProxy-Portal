package agy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCLIDiagnosticPreservesOriginalCaseAndLines(t *testing.T) {
	raw := "RESOURCE_EXHAUSTED (code 429): Retry Failed\r\n  at agent.Run(worker.go:123)\r\n  model=Gemini-Test\r\n"
	failure := mapRPCError(&rpcError{Code: -32603, Message: raw}).(*Error)
	if failure.Diagnostic != raw {
		t.Fatal("CLI diagnostic case, whitespace or stack lines changed")
	}
	w := httptest.NewRecorder()
	WriteError(w, failure)
	_, message := diagnosticErrorMessage(t, w.Body.String(), false)
	if message != failure.Message || len(w.Result().Trailer) != 0 {
		t.Fatal("public error writer exposed private diagnostics")
	}
	encoded, _ := json.Marshal(failure)
	if strings.Contains(failure.Error(), "worker.go") || strings.Contains(string(encoded), "worker.go") || strings.Contains(FailureSummary(failure.Code), "worker.go") {
		t.Fatal("private diagnostic entered generic error serialization")
	}
	unknown := mapRPCError(&rpcError{Code: -32603, Message: "Unrecognized CLI error\n  Details: unexpected worker result"}).(*Error)
	if unknown.Code != "agent_error" || !strings.Contains(unknown.Diagnostic, "Unrecognized CLI error\n  Details:") {
		t.Fatal("unknown diagnostic was still discarded")
	}
	validation := fail(400, "invalid_instructions", "instructions 必须为文本").(*Error)
	if validation.Diagnostic != "" {
		t.Fatal("local validation invented a CLI diagnostic")
	}
}

func TestCLIDiagnosticRedactsCredentialForms(t *testing.T) {
	for _, raw := range []string{
		"Authorization: Bearer secret-credential\nRESOURCE_EXHAUSTED: quota reached",
		"Proxy-Authorization: Basic secret-credential\nRESOURCE_EXHAUSTED: quota reached",
		"Cookie: sid=secret-credential; csrf=another-secret\nRESOURCE_EXHAUSTED: quota reached",
		`RESOURCE_EXHAUSTED: {"access_token":"secret-credential","refresh_token":"another-secret","code":429}`,
		`RESOURCE_EXHAUSTED: {\"access_token\":\"secret-credential\",\"refresh_token\":\"another-secret\"}`,
		"RESOURCE_EXHAUSTED: client_secret='secret-credential' password=another-secret",
		"RESOURCE_EXHAUSTED: Bearer secret-credential",
		"RESOURCE_EXHAUSTED: https://host.test/path?code=secret-credential&key=another-secret",
		"RESOURCE_EXHAUSTED: https://name:secret-credential@host.test/path",
		"RESOURCE_EXHAUSTED: https://host.test/path?X-Goog-Signature=secret-credential&X-Amz-Credential=another-secret",
		`RESOURCE_EXHAUSTED: {"token":"[redacted] secret-credential"}`,
		"RESOURCE_EXHAUSTED: sk-secret-credential-12345678 AIzasecret-credential-12345678",
	} {
		safe := sanitizeCLIDiagnostic(raw)
		if strings.Contains(safe, "secret-credential") || strings.Contains(safe, "another-secret") || !strings.Contains(safe, "RESOURCE_EXHAUSTED") || !strings.Contains(safe, "[redacted]") {
			t.Fatal("diagnostic credentials were exposed or reason removed")
		}
	}
}

func TestCLIDiagnosticLimitIsExplicitAndKeepsUTF8AndFinalReason(t *testing.T) {
	raw := "Original CLI failure\n" + strings.Repeat("诊断\n", maxCLIDiagnosticBytes) + "\nAuthorization: Bearer secret-credential\nFinal RESOURCE_EXHAUSTED"
	safe := sanitizeCLIDiagnostic(raw)
	if len(safe) > maxCLIDiagnosticBytes || !utf8.ValidString(safe) || !strings.Contains(safe, diagnosticTruncation) || !strings.HasPrefix(safe, "Original CLI failure") || !strings.HasSuffix(safe, "Final RESOURCE_EXHAUSTED") || strings.Contains(safe, "secret-credential") {
		t.Fatal("oversized diagnostic was unbounded, invalid or silently cut")
	}
	var combined error
	for i := 0; i < 100; i++ {
		next := withCLIDiagnostic(&Error{Status: 429, Code: "upstream_resource_exhausted", Message: "限流"}, fmt.Sprintf("Attempt %d\n%s", i, strings.Repeat("x", 2048)))
		combined = retainPromptWarning(combined, next)
	}
	if len(combined.(*Error).Diagnostic) > maxCLIDiagnosticBytes || !strings.Contains(combined.(*Error).Diagnostic, "Attempt 99") {
		t.Fatal("retry diagnostics grew without bound or lost the final attempt")
	}
}

func diagnosticErrorMessage(t *testing.T, body string, stream bool) (string, string) {
	t.Helper()
	var v struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if !stream {
		if json.Unmarshal([]byte(body), &v) != nil {
			t.Fatal("invalid error JSON")
		}
		return v.Error.Code, v.Error.Message
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Type == "response.failed" {
			if json.Unmarshal(event.Response, &v) != nil {
				t.Fatal("invalid failed response")
			}
			return v.Error.Code, v.Error.Message
		}
	}
	t.Fatal("missing failed response")
	return "", ""
}

func TestFinalCLIDiagnosticWaitsForRetriesAndReachesPortalOnly(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, rpcFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/rpc=%v", stream, rpcFailure), func(t *testing.T) {
				warned, resume := make(chan struct{}), make(chan struct{})
				var release sync.Once
				t.Cleanup(func() { release.Do(func() { close(resume) }) })
				var calls atomic.Int64
				var m *Manager
				m, _ = recoveryFixture(t, func(s *session, f frame) any {
					calls.Add(1)
					for i := 1; i <= 3; i++ {
						retryTestText(m, s, fmt.Sprintf("⚠ API error (attempt %d): RESOURCE_EXHAUSTED\n  Retry Detail %d\n  Authorization: Bearer secret-credential", i, i), false)
					}
					close(warned)
					<-resume
					if rpcFailure {
						return &rpcError{Code: -32603, Message: "Agent Final RPC failure: RESOURCE_EXHAUSTED\n  Native Stack: worker.go:123"}
					}
					retryTestText(m, s, retryTestTermination, false)
					return map[string]any{"stopReason": "end_turn"}
				})
				body, _ := json.Marshal(map[string]any{"model": "agy/test", "previous_response_id": "previous", "input": "continue", "stream": stream})
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- recoveryRequest(m, string(body)) }()
				select {
				case <-warned:
				case <-time.After(time.Second):
					t.Fatal("prompt did not start")
				}
				select {
				case <-done:
					t.Fatal("intermediate retry warning ended the request")
				case <-time.After(25 * time.Millisecond):
				}
				release.Do(func() { close(resume) })
				var response *httptest.ResponseRecorder
				select {
				case response = <-done:
				case <-time.After(time.Second):
					t.Fatal("native completion did not end the request")
				}
				code, clientMessage := diagnosticErrorMessage(t, response.Body.String(), stream)
				if clientMessage != publicFailureMessage(code) || strings.Contains(response.Body.String(), "Retry Detail") || strings.Contains(response.Body.String(), "worker.go") {
					t.Fatal("client lost its original summary or received CLI diagnostics")
				}
				var detail struct {
					Diagnostic string `json:"cli_diagnostic"`
				}
				if json.Unmarshal([]byte(ResponseFailureSummary(code, response.Result())), &detail) != nil {
					t.Fatal("invalid portal diagnostic summary")
				}
				message := detail.Diagnostic
				for _, want := range []string{"Retry Detail 1", "Retry Detail 2", "Retry Detail 3", "[redacted]"} {
					if !strings.Contains(message, want) {
						t.Fatal("final diagnostic omitted retry details", want)
					}
				}
				if code != "upstream_resource_exhausted" || calls.Load() != 1 || strings.Contains(message, "secret-credential") || strings.Contains(response.Body.String(), `"status":"completed"`) {
					t.Fatal("diagnostic changed retries, status or exposed credentials")
				}
				if strings.Contains(message, "AGY CLI diagnostic:") || strings.Contains(message, "上游额度不足") {
					t.Fatal("portal diagnostic was prefixed with a translated explanation")
				}
				if rpcFailure && !strings.Contains(message, "Native Stack: worker.go:123") || !rpcFailure && !strings.Contains(message, "Agent execution terminated due to error.") {
					t.Fatal("final native diagnostic was not preserved")
				}
			})
		}
	}
}

func TestPrivateDiagnosticTransportHandlesLargeJSONAndSSEWithReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, replay := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/replay=%v", stream, replay), func(t *testing.T) {
				diagnostic := "RESOURCE_EXHAUSTED\n" + strings.Repeat("detail line\n", 6000) + "Native Final Stack"
				failure := withCLIDiagnostic(&Error{Status: 429, Code: "upstream_resource_exhausted", Message: publicFailureMessage("upstream_resource_exhausted")}, diagnostic)
				m := &Manager{life: t.Context()}
				execute := func(w http.ResponseWriter, r *http.Request) {
					if !stream {
						writeWorkerError(w, failure)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
					http.NewResponseController(w).Flush()
					out := &response{w: w, started: true, stream: true}
					out.fail(failure)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if replay {
						m.replayResponse(w, r, execute)
					} else {
						execute(w, r)
					}
				}))
				defer server.Close()
				client, err := NewClient(server.URL, strings.Repeat("s", 64))
				if err != nil {
					t.Fatal(err)
				}
				ctx := WithRequestIdentity(t.Context(), http.Header{"Idempotency-Key": {"diagnostic-transport"}})
				resp, err := client.Request(ctx, "POST", "/internal/responses", "owner", strings.NewReader(`{"model":"agy/test","input":"test"}`))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				code, message := diagnosticErrorMessage(t, string(body), stream)
				if message != failure.Message || strings.Contains(string(body), "Native Final Stack") {
					t.Fatal("private diagnostic entered public body")
				}
				var detail struct {
					Diagnostic string `json:"cli_diagnostic"`
				}
				json.Unmarshal([]byte(ResponseFailureSummary(code, resp)), &detail)
				if detail.Diagnostic != failure.Diagnostic {
					t.Fatalf("private transport lost diagnostic: got %d bytes, want %d", len(detail.Diagnostic), len(failure.Diagnostic))
				}
			})
		}
	}
}

func TestPortalDiagnosticMetadataRejectsMalformedTrailerAndRedactsAgain(t *testing.T) {
	for _, encoded := range []string{"", "%%%invalid", strings.Repeat("A", maxCLIDiagnosticBytes*2)} {
		resp := &http.Response{Trailer: http.Header{cliDiagnosticTrailer: {encoded}}}
		if ResponseFailureSummary("agent_error", resp) != FailureSummary("agent_error") {
			t.Fatal("invalid trailer did not fall back to the existing summary")
		}
	}
}
