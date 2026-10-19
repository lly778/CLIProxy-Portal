package httpserver

import (
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

func TestRequestFailureSummaryHidesSuccessMetadata(t *testing.T) {
	event := cpamp.EventRow{Failed: false, FailSummary: `{"Cf-Cache-Status":["DYNAMIC"],"Cf-Ray":["abc"]}`}
	if got := requestFailureText(event); got != "" {
		t.Fatalf("success summary = %q, want empty", got)
	}
}

func TestRequestLogMessagesUseEnglishBelowChineseStatus(t *testing.T) {
	for _, test := range []struct{ stored, want string }{
		{"全局并发已满", "Global concurrency limit reached"},
		{"用户并发已满", "User concurrency limit reached"},
		{"交互记录待处理过多", "Interaction capture backlog limit reached"},
		{"整机内存达到总上限", "Server memory limit reached"},
		{"CPU 持续繁忙", "Server CPU limit exceeded"},
		{"资源监测未连接", "Resource monitor unavailable"},
		{"请求超过大小上限", "Request size limit exceeded"},
		{"请求过大或内容无效", "Request too large or invalid"},
		{"Upstream request failed", "Upstream request failed"},
		{"Connection refused", "Connection refused"},
	} {
		t.Run(test.stored, func(t *testing.T) {
			status := int64(503)
			event := cpamp.EventRow{PortalResourceID: 1, Failed: true, FailStatusCode: &status, FailSummary: test.stored}
			s := &Server{}
			// A local resource event still needs a valid display time zone.
			s.Cfg.TimeZone = time.UTC
			view := s.requestView(event)
			if view.StatusLabel != "已拦截" || view.Error != "HTTP 503 · "+test.want || view.ErrorFull != view.Error {
				t.Fatalf("blocked display: %+v", view)
			}
			event.PortalResourceObserved, event.Failed = true, false
			view = s.requestView(event)
			if view.StatusLabel != "观察放行" || view.Error != test.want || view.ErrorFull != view.Error {
				t.Fatalf("observed display: %+v", view)
			}
			if event.FailSummary != test.stored {
				t.Fatal("display translation changed original metadata")
			}
		})
	}
}

func TestRequestModelLabel(t *testing.T) {
	tests := []struct {
		name  string
		event cpamp.EventRow
		want  string
	}{
		{"alias routed to real model", cpamp.EventRow{Model: "gpt-5.6-luna", RequestedModel: "gpt-5.6-luna", ResolvedModel: "gpt-6-luna"}, "gpt-5.6-luna → gpt-6-luna"},
		{"same name is not repeated", cpamp.EventRow{Model: "gpt-6-luna", RequestedModel: "gpt-6-luna", ResolvedModel: "gpt-6-luna"}, "gpt-6-luna"},
		{"older event without route", cpamp.EventRow{Model: "gpt-5.6-luna"}, "gpt-5.6-luna"},
		{"missing requested model", cpamp.EventRow{ResolvedModel: "gpt-6-luna"}, "gpt-6-luna"},
		{"model fallback retains route", cpamp.EventRow{Model: "gpt-5.6-luna", ResolvedModel: "gpt-6-luna"}, "gpt-5.6-luna → gpt-6-luna"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestModelLabel(tt.event); got != tt.want {
				t.Fatalf("requestModelLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReasoningEffortLabelKeepsXHighBelowMax(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "high", want: "高"},
		{input: "xhigh", want: "极高"},
		{input: "max", want: "最高"},
	} {
		if got := reasoningEffortLabel(test.input); got != test.want {
			t.Errorf("reasoningEffortLabel(%q) = %q, want %q", test.input, got, test.want)
		}
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
