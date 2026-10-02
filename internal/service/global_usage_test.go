package service

import (
	"context"
	"reflect"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

type globalUsageCPAMP struct {
	cpamp.API
	value   cpamp.AnalyticsResponse
	calls   int
	request cpamp.AnalyticsRequest
}

func (f *globalUsageCPAMP) Analytics(_ context.Context, req cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	f.calls++
	f.request = req
	return f.value, nil
}

func TestGlobalUsagePreservesRequestStatusSummaryAndCache(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	want := cpamp.UsageSummary{TotalCalls: 3373, SuccessCalls: 3358, FailureCalls: 15, SuccessRate: 3358.0 / 3373, TotalTokens: 1234567}
	for _, tc := range []struct {
		name  string
		stats []cpamp.APIKeyUsageStat
	}{
		{"conflicting complete key aggregates", []cpamp.APIKeyUsageStat{{Calls: 3373, SuccessCalls: 2696, FailureCalls: 677}}},
		{"missing keys", nil},
		{"fewer calls", []cpamp.APIKeyUsageStat{{Calls: 3000, SuccessCalls: 2990, FailureCalls: 10}}},
		{"unclassified calls", []cpamp.APIKeyUsageStat{{Calls: 3373, SuccessCalls: 2696, FailureCalls: 15}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := want
			fake := &globalUsageCPAMP{value: cpamp.AnalyticsResponse{Summary: &summary, APIKeyStats: tc.stats, Timeline: []cpamp.UsageTimelinePoint{{Calls: 3373, Success: 3358, Failure: 15}}}}
			keys := NewKeys(nil, fake)
			for i := 0; i < 2; i++ {
				value, err := keys.GlobalUsage(t.Context(), from, from.Add(24*time.Hour), 0)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(*value.Summary, want) || value.Timeline[0].Failure != value.Summary.FailureCalls {
					t.Fatalf("summary no longer matches request timeline: %+v", value)
				}
				if !reflect.DeepEqual(value.APIKeyStats, tc.stats) {
					t.Fatal("per-key ranking data changed")
				}
			}
			if fake.calls != 1 || !fake.request.Include.Summary || !fake.request.Include.Timeline || !fake.request.Include.APIKeyStats {
				t.Fatalf("global analytics request/cache changed: calls=%d request=%+v", fake.calls, fake.request)
			}
		})
	}
}
