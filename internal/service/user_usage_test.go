package service

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

func TestUserUsageStatusUsesCompleteTimelineAndCachesCorrection(t *testing.T) {
	accounts, st := accountsForTest(t)
	u, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	for i, status := range []string{"revoked", "active"} {
		if err := st.CreateKey(t.Context(), domain.APIKey{ID: status, UserID: u.ID, Hash: strings.Repeat(string(rune('a'+i)), 64), Status: status, IssuedAt: from}); err != nil {
			t.Fatal(err)
		}
	}
	summary := cpamp.UsageSummary{TotalCalls: 503, SuccessCalls: 463, FailureCalls: 40, TotalTokens: 65400000}
	want := summary
	want.SuccessCalls, want.FailureCalls, want.SuccessRate = 502, 1, 502.0/503
	fake := &globalUsageCPAMP{value: cpamp.AnalyticsResponse{
		Summary: &summary,
		Timeline: []cpamp.UsageTimelinePoint{
			{BucketMS: from.UnixMilli(), Calls: 300, Success: 299, Failure: 1},
			{BucketMS: from.Add(time.Hour).UnixMilli(), Calls: 203, Success: 203},
		},
		Events: &cpamp.EventsResponse{Items: []cpamp.EventRow{{Failed: true}}},
	}}
	keys := NewKeys(st, fake)
	for _, refresh := range []bool{false, false, true, false} {
		got, err := keys.usage(t.Context(), u.ID, from, from.Add(7*24*time.Hour), 100, refresh)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*got.Summary, want) {
			t.Fatalf("user summary = %+v, want %+v", *got.Summary, want)
		}
		if !reflect.DeepEqual(got.Timeline, fake.value.Timeline) {
			t.Fatal("status correction must not modify trend data")
		}
	}
	if fake.calls != 2 || len(fake.request.Filters.APIKeyHashes) != 2 || !fake.request.Include.Timeline {
		t.Fatalf("user history/cache query changed: calls=%d request=%+v", fake.calls, fake.request)
	}
	if summary.SuccessCalls != 463 || summary.FailureCalls != 40 {
		t.Fatal("correction mutated the source summary")
	}
}

func TestUserUsageStatusKeepsSummaryWithoutCompleteTimeline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timeline []cpamp.UsageTimelinePoint
	}{
		{"missing timeline", nil},
		{"partial timeline", []cpamp.UsageTimelinePoint{{Calls: 100, Success: 99, Failure: 1}}},
		{"unclassified bucket", []cpamp.UsageTimelinePoint{{Calls: 503, Success: 502}}},
		{"negative status", []cpamp.UsageTimelinePoint{{Calls: 503, Success: 504, Failure: -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := cpamp.UsageSummary{TotalCalls: 503, SuccessCalls: 463, FailureCalls: 40}
			got := usageStatusFromTimeline(cpamp.AnalyticsResponse{Summary: &want, Timeline: tc.timeline})
			if !reflect.DeepEqual(*got.Summary, want) {
				t.Fatal("incomplete timeline must not replace summary counts")
			}
		})
	}
	if got := usageStatusFromTimeline(cpamp.AnalyticsResponse{Timeline: []cpamp.UsageTimelinePoint{{Calls: 1, Success: 1}}}); got.Summary != nil {
		t.Fatal("status correction must not invent a missing summary")
	}
}
