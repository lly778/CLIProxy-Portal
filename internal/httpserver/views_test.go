package httpserver

import (
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

func TestRequestFailureSummaryHidesSuccessMetadata(t *testing.T) {
	event := cpamp.EventRow{Failed: false, FailSummary: `{"Cf-Cache-Status":["DYNAMIC"],"Cf-Ray":["abc"]}`}
	if got := requestFailureText(event); got != "" {
		t.Fatalf("success summary = %q, want empty", got)
	}
}

func TestRequestFailureSummaryExtractsUsefulFailureDetails(t *testing.T) {
	status := int64(429)
	event := cpamp.EventRow{
		Failed:         true,
		FailStatusCode: &status,
		FailSummary:    `{"error":{"type":"usage_limit_reached","message":"Usage limit reached"},"Cf-Ray":["abc"]}`,
	}
	if got, want := requestFailureText(event), "HTTP 429 · Usage limit reached"; got != want {
		t.Fatalf("failure summary = %q, want %q", got, want)
	}
}

func TestRequestFailureSummaryHidesHeaderOnlyJSON(t *testing.T) {
	status := int64(502)
	event := cpamp.EventRow{Failed: true, FailStatusCode: &status, FailSummary: `{"Cf-Cache-Status":["DYNAMIC"],"Cf-Ray":["abc"]}`}
	if got, want := requestFailureText(event), "HTTP 502"; got != want {
		t.Fatalf("header-only failure summary = %q, want %q", got, want)
	}
}

func TestRequestFailureSummaryReadsReasonAfterConcatenatedHeaders(t *testing.T) {
	status := int64(429)
	event := cpamp.EventRow{
		Failed:         true,
		FailStatusCode: &status,
		FailSummary:    `{"Cf-Cache-Status":["DYNAMIC"],"Cf-Ray":["abc"]}{"type":"usage_limit_reached","message":"Usage limit reached"}`,
	}
	if got, want := requestFailureText(event), "HTTP 429 · Usage limit reached"; got != want {
		t.Fatalf("concatenated failure summary = %q, want %q", got, want)
	}
}

func TestRequestFailureSummaryFallsBackToErrorCode(t *testing.T) {
	status := int64(429)
	event := cpamp.EventRow{Failed: true, FailStatusCode: &status, FailSummary: `{"type":"usage_limit_reached"}`}
	if got, want := requestFailureText(event), "HTTP 429 · usage_limit_reached"; got != want {
		t.Fatalf("code-only failure summary = %q, want %q", got, want)
	}
}

func TestRequestFailureSummaryKeepsPlainErrorBeforeMetadata(t *testing.T) {
	status := int64(503)
	event := cpamp.EventRow{
		Failed:         true,
		FailStatusCode: &status,
		FailSummary:    "upstream connect error: Connection refused\n" + `{"Cf-Cache-Status":["DYNAMIC"],"Set-Cookie":["secret"]}`,
	}
	if got, want := requestFailureText(event), "HTTP 503 · upstream connect error: Connection refused"; got != want {
		t.Fatalf("plain failure with metadata = %q, want %q", got, want)
	}
}

func TestRequestFailureSummaryKeepsPlainErrorAfterMetadata(t *testing.T) {
	status := int64(503)
	event := cpamp.EventRow{
		Failed:         true,
		FailStatusCode: &status,
		FailSummary:    `{"Cf-Cache-Status":["DYNAMIC"],"Set-Cookie":["secret"]}` + "\nupstream connect error: Connection refused",
	}
	if got, want := requestFailureText(event), "HTTP 503 · upstream connect error: Connection refused"; got != want {
		t.Fatalf("metadata followed by plain failure = %q, want %q", got, want)
	}
}

func TestRequestFailureTextKeepsFullErrorForTooltip(t *testing.T) {
	status := int64(503)
	original := strings.Repeat("connection failed ", 20) + "Connection refused"
	event := cpamp.EventRow{
		Failed:         true,
		FailStatusCode: &status,
		FailSummary:    original + "\n" + `{"Cf-Cache-Status":["DYNAMIC"],"Set-Cookie":["secret"]}`,
	}

	full := requestFailureText(event)
	if !strings.Contains(full, "Connection refused") {
		t.Fatalf("full failure text = %q, want final reason", full)
	}
	if strings.Contains(full, "Set-Cookie") || strings.Contains(full, "secret") {
		t.Fatalf("full failure text leaked response metadata: %q", full)
	}
}
