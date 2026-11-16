package agy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUpstreamFailureReasonsRemainSpecificAndPrivate(t *testing.T) {
	for _, tc := range []struct{ diagnostic, code string }{
		{"UNAVAILABLE(code 503): No capacity available for model gemini on the server", "upstream_capacity_exhausted"},
		{"The model is overloaded", "upstream_overloaded"},
		{"RESOURCE_EXHAUSTED: request quota exceeded", "upstream_resource_exhausted"},
		{"PERMISSION_DENIED: access denied", "upstream_access_denied"},
		{"DEADLINE_EXCEEDED: upstream request timed out", "upstream_deadline_exceeded"},
		{"stream idle timeout (5m0s): context deadline exceeded", "upstream_stream_idle_timeout"},
		{"INVALID_ARGUMENT: bad request", "upstream_invalid_argument"},
		{"Tool is not allowed or its call has expired.", "upstream_tool_call_expired"},
		{"UNAUTHENTICATED: token has expired", "auth_required"},
		{"This service has been disabled in this account", "account_disabled"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			text := tc.diagnostic + " https://example.invalid/?token=private-token user@private.invalid Bearer secret-credential prompt-content"
			for _, err := range []error{agentFailure("⚠ API error: " + text), mapRPCError(&rpcError{Code: -32603, Message: text})} {
				failure, ok := err.(*Error)
				if !ok || failure.Code != tc.code {
					t.Fatalf("lost upstream reason: %v", err)
				}
				public := failure.Message + FailureSummary(failure.Code)
				for _, private := range []string{"private-token", "private.invalid", "secret-credential", "prompt-content"} {
					if strings.Contains(public, private) {
						t.Fatalf("diagnostic leaked %q", private)
					}
				}
			}
			if agentFailure(tc.diagnostic) != nil {
				t.Fatal("ordinary assistant text became a provider failure")
			}
		})
	}
	unknown := mapRPCError(&rpcError{Code: -32603, Message: "private unexpected diagnostic"}).(*Error)
	if unknown.Code != "agent_error" || strings.Contains(unknown.Message, "private") {
		t.Fatal("unknown diagnostic was exposed or misclassified")
	}
}

func TestTrackerReadsFailureReasonForJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, code := range []string{"session_busy", "compaction_tools_pending", "missing_tool_result", "upstream_capacity_exhausted", "upstream_stream_idle_timeout", "upstream_resource_exhausted", "agent_execution_failed", "auth_required", "private-unknown-code"} {
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": "private provider diagnostic"}})
			if stream {
				body = []byte("data: {\"type\":\"response.failed\",\"response\":" + string(body) + "}\n\n")
			}
			tracker := &Tracker{Stream: stream}
			for _, b := range body {
				tracker.Feed([]byte{b})
			}
			tracker.Finish()
			want := code
			if code == "private-unknown-code" {
				want = "agy_generation_failed"
			}
			if tracker.Failure != want || tracker.Completed || tracker.Usage.Known {
				t.Fatalf("stream=%v code=%s tracker=%+v", stream, code, tracker)
			}
			if strings.Contains(FailureSummary(tracker.Failure), "private") {
				t.Fatal("raw provider error entered log summary")
			}
		}
	}
}

func TestLocalValidationReasonsSurviveJSONAndSSEWithoutPrivateData(t *testing.T) {
	for code, local := range validationFailures {
		for _, stream := range []bool{false, true} {
			body, _ := json.Marshal(map[string]any{"status": "failed", "error": map[string]string{"code": code, "message": "private-instructions https://private.invalid/?token=secret-credential"}})
			if stream {
				body = []byte("data: {\"type\":\"response.failed\",\"response\":" + string(body) + "}\n\n")
			}
			tracker := &Tracker{Stream: stream}
			tracker.Feed(body)
			tracker.Finish()
			if tracker.Completed || tracker.Failure != code || FailureStatus(tracker.Failure) != local.status {
				t.Fatalf("stream=%v code=%s lost local failure: %+v", stream, code, tracker)
			}
			var summary struct{ Code, Message string }
			encoded := FailureSummary(tracker.Failure)
			if json.Unmarshal([]byte(encoded), &summary) != nil || summary.Code != code || summary.Message != local.message || strings.Contains(encoded, "private") || strings.Contains(encoded, "secret-credential") {
				t.Fatal("summary lost the local reason or leaked input", encoded)
			}
		}
	}
}

func TestEarlyInputValidationReturnsSpecificErrorWithoutModelCall(t *testing.T) {
	for _, tc := range []struct{ code, body string }{
		{"invalid_instructions", `{"model":"agy/test","instructions":[],"input":"continue"}`},
		{"opaque_context_unsupported", `{"model":"agy/test","input":[{"type":"reasoning","encrypted_content":"private-reasoning"}]}`},
		{"unsupported_input_item", `{"model":"agy/test","input":[{"type":"unsupported-kind","content":"private-input"}]}`},
		{"invalid_role", `{"model":"agy/test","input":[{"role":"invalid","content":"private-input"}]}`},
		{"image_url_unsupported", `{"model":"agy/test","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://private.invalid/picture"}]}]}`},
	} {
		t.Run(tc.code, func(t *testing.T) {
			m, s := recoveryFixture(t, func(*session, frame) any { t.Error("invalid input started a model prompt"); return nil })
			before := digest(snapshotSession(s))
			w := recoveryRequest(m, tc.body)
			tracker := &Tracker{}
			tracker.Feed(w.Body.Bytes())
			tracker.Finish()
			if w.Code != 400 || tracker.Failure != tc.code || tracker.Completed || digest(snapshotSession(s)) != before || len(m.sessions) != 1 {
				t.Fatalf("local validation was relabeled or mutated the session: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestTrackerErrorEnvelopeCannotRemainCompleted(t *testing.T) {
	tracker := &Tracker{}
	tracker.Feed([]byte(`{"status":"completed","error":{"code":"agent_execution_failed"}}`))
	tracker.Finish()
	if tracker.Completed || tracker.Failure != "agent_execution_failed" {
		t.Fatal("an error envelope was counted as completed", tracker)
	}
}
