package httpserver

import (
	"testing"

	"cliproxy-portal/internal/cpamp"
)

func TestRequestFailureSummaryHidesSuccessMetadata(t *testing.T) {
	event := cpamp.EventRow{Failed: false, FailSummary: `{"Cf-Cache-Status":["DYNAMIC"],"Cf-Ray":["abc"]}`}
	if got := requestFailureSummary(event); got != "" {
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
	if got, want := requestFailureSummary(event), "HTTP 429 · usage_limit_reached · Usage limit reached"; got != want {
		t.Fatalf("failure summary = %q, want %q", got, want)
	}
}

func TestRequestFailureSummaryHidesHeaderOnlyJSON(t *testing.T) {
	status := int64(502)
	event := cpamp.EventRow{Failed: true, FailStatusCode: &status, FailSummary: `{"Cf-Cache-Status":["DYNAMIC"],"Cf-Ray":["abc"]}`}
	if got, want := requestFailureSummary(event), "HTTP 502"; got != want {
		t.Fatalf("header-only failure summary = %q, want %q", got, want)
	}
}
