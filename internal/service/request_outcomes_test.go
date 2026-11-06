package service

import (
	"context"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

func TestRequestLogStatusDeduplicatesAndPreservesTokens(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	at := now.UnixMilli()
	k := NewKeys(nil, nil)
	k.UsageTimingLocation = time.UTC
	req := cpamp.AnalyticsRequest{FromMS: at, ToMS: at + 3600000, Include: cpamp.AnalyticsInclude{Summary: true, Timeline: true, Granularity: "hour"}}
	summary := &cpamp.UsageSummary{TotalCalls: 5, SuccessCalls: 4, FailureCalls: 1, TotalTokens: 99}
	e := domain.RequestOutcomeEvent{Hash: "one", AtMS: at + 1000}
	value := cpamp.AnalyticsResponse{Summary: summary, Timeline: []cpamp.UsageTimelinePoint{{BucketMS: at, Calls: 5, Success: 4, Failure: 1, TotalTokens: 99}}, RequestLogData: &domain.RequestLogData{Failures: []domain.GatewayFailure{
		{ID: "a", UserID: "user", RequestID: "request", APIKeyHash: "key", Upstream: []domain.RequestOutcomeEvent{e}},
		{ID: "duplicate", UserID: "user", RequestID: "request", APIKeyHash: "key", Upstream: []domain.RequestOutcomeEvent{e}},
		{ID: "already-failed", UserID: "user", RequestID: "failed", APIKeyHash: "key", Upstream: []domain.RequestOutcomeEvent{{Hash: "failed", AtMS: at + 2000, Failed: true}}},
		{ID: "not-forwarded", UserID: "user", StartedMS: at + 3000},
	}, Rejections: []domain.RequestRejection{{UserID: "user", AtMS: at + 4000, Calls: 2}}}}
	got := k.withRequestLogStatus(value, req)
	if got.RequestLogNote != "" || got.Summary.TotalCalls != 8 || got.Summary.SuccessCalls != 3 || got.Summary.FailureCalls != 5 || got.Summary.TotalTokens != 99 {
		t.Fatalf("merged summary=%+v note=%s", got.Summary, got.RequestLogNote)
	}
	if got.Timeline[0].Calls != 8 || got.Timeline[0].Success != 3 || got.Timeline[0].Failure != 5 || got.Timeline[0].TotalTokens != 99 {
		t.Fatalf("timeline disagrees: %+v", got.Timeline)
	}
	if summary.TotalCalls != 5 || value.Timeline[0].Calls != 5 {
		t.Fatal("raw cached source mutated")
	}
	if len(got.RequestLogUsers) != 1 || got.RequestLogUsers[0].Calls != 3 {
		t.Fatal("user ranking did not include standalone failures")
	}
}

type failureReconcileCPAMP struct {
	cpamp.API
	queries []cpamp.AnalyticsRequest
	events  []cpamp.EventRow
}

func (f *failureReconcileCPAMP) Analytics(_ context.Context, req cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	f.queries = append(f.queries, req)
	return cpamp.AnalyticsResponse{GeneratedAtMS: 100, Events: &cpamp.EventsResponse{Items: f.events}}, nil
}

func TestFailureReconciliationUsesOnlyAffectedWindowAndExactIDs(t *testing.T) {
	_, st := accountsForTest(t)
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	at := now.UnixMilli()
	fake := &failureReconcileCPAMP{events: []cpamp.EventRow{{EventHash: "event", RequestID: "request", APIKeyHash: "key", TimestampMS: at + 1000}, {EventHash: "other", RequestID: "unrelated", APIKeyHash: "key", TimestampMS: at + 2000}}}
	k := NewKeys(st, fake)
	k.Now = func() time.Time { return now }
	data := domain.RequestLogData{Failures: []domain.GatewayFailure{{ID: "failure", UserID: "a", APIKeyHash: "key", RequestID: "request", StartedMS: at, EndedMS: at + 61000}}}
	if err := k.reconcileRequestFailures(t.Context(), &data, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.queries) != 1 || len(fake.queries[0].Filters.APIKeyHashes) != 1 || fake.queries[0].Filters.APIKeyHashes[0] != "key" || fake.queries[0].Include.Summary || fake.queries[0].Include.Timeline || fake.queries[0].FromMS != at-1000 || fake.queries[0].ToMS != at+66000 {
		t.Fatalf("partial query widened: %+v", fake.queries)
	}
	if len(data.Failures[0].Upstream) != 1 || data.Failures[0].Upstream[0].Hash != "event" {
		t.Fatal("matched a different request")
	}
	if err := k.reconcileRequestFailures(t.Context(), &data, false); err != nil || len(fake.queries) != 1 {
		t.Fatal("known reconciliation was queried again")
	}
}

func TestFailureReconciliationReplacesStandaloneFailureWhenUpstreamArrives(t *testing.T) {
	_, st := accountsForTest(t)
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	at := now.UnixMilli()
	fake := &failureReconcileCPAMP{}
	k := NewKeys(st, fake)
	k.Now = func() time.Time { return now }
	data := domain.RequestLogData{Failures: []domain.GatewayFailure{{ID: "late", UserID: "a", APIKeyHash: "key", RequestID: "request", StartedMS: at, EndedMS: at + 61000}}}
	if err := k.reconcileRequestFailures(t.Context(), &data, false); err != nil {
		t.Fatal(err)
	}
	deltas := requestLogDeltas(&data, at, at+3600000)
	if len(deltas) != 1 || deltas[0].calls != 1 || deltas[0].failed != 1 {
		t.Fatal("unmatched gateway failure was lost")
	}
	now = now.Add(2 * time.Minute)
	fake.events = []cpamp.EventRow{{EventHash: "late-event", RequestID: "request", APIKeyHash: "key", TimestampMS: at + 1000}}
	if err := k.reconcileRequestFailures(t.Context(), &data, false); err != nil {
		t.Fatal(err)
	}
	deltas = requestLogDeltas(&data, at, at+3600000)
	if len(fake.queries) != 2 || len(deltas) != 1 || deltas[0].calls != 0 || deltas[0].success != -1 || deltas[0].failed != 1 {
		t.Fatal("late upstream row was counted as another request")
	}
}
