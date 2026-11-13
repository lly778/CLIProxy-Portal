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
		for _, code := range []string{"session_busy", "compaction_tools_pending", "missing_tool_result", "upstream_capacity_exhausted", "upstream_stream_idle_timeout", "auth_required", "private-unknown-code"} {
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
