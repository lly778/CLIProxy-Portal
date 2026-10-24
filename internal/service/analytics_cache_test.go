package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

func TestOverviewQueriesNeverWalkFullModelHistory(t *testing.T) {
	accounts, st := accountsForTest(t)
	u, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 12, 59, 59, 0, time.UTC)
	hash := strings.Repeat("a", 64)
	if err := st.CreateKey(t.Context(), domain.APIKey{ID: "key", UserID: u.ID, Hash: hash, Status: "active", IssuedAt: now}); err != nil {
		t.Fatal(err)
	}
	fake := &globalUsageCPAMP{value: cpamp.AnalyticsResponse{
		Summary:     &cpamp.UsageSummary{TotalCalls: 2, TotalTokens: 30},
		Timeline:    []cpamp.UsageTimelinePoint{{Calls: 2, Success: 1, Failure: 1}},
		APIKeyStats: []cpamp.APIKeyUsageStat{{APIKeyHash: hash, Calls: 2}},
		Events: &cpamp.EventsResponse{HasMore: true, NextBeforeMS: now.UnixMilli(), NextBeforeID: 1,
			Items: []cpamp.EventRow{{APIKeyHash: hash, ResolvedModel: "actual", TotalTokens: 10}}},
	}}
	k := NewKeys(st, fake)
	k.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		value, err := k.UserOverviewUsage(t.Context(), u.ID, now.Add(-7*24*time.Hour), now)
		if err != nil || value.Summary.FailureCalls != 1 || len(value.Events.Items) != 1 {
			t.Fatal("account overview lost summary or preview", err)
		}
		if fake.calls != 1 || fake.request.Include.ModelStats || fake.request.Include.EventsPage.Limit != 10 || len(fake.request.Filters.APIKeyHashes) != 1 {
			t.Fatal("account overview queried unnecessary history")
		}
		now = now.Add(2 * time.Second)
	}
	value, err := k.GlobalOverviewUsage(t.Context(), now.Add(-7*24*time.Hour), now)
	if err != nil || len(value.APIKeyStats) != 1 || value.Events != nil {
		t.Fatal("global overview lost rankings", err)
	}
	if fake.calls != 2 || fake.request.Include.EventsPage != nil || fake.request.Include.Timeline || fake.request.Include.ModelStats || !fake.request.Include.APIKeyStats || len(fake.request.Filters.APIKeyHashes) != 0 {
		t.Fatal("global overview queried model history or reused a user scope")
	}
	// Full usage must not reuse the overview's incomplete model statistics.
	fake.value.Summary.TotalCalls = 1
	fake.value.Summary.TotalTokens = 10
	fake.value.Events.HasMore = false
	full, err := k.GlobalUsage(t.Context(), now.Add(-7*24*time.Hour), now, 0)
	if err != nil || fake.calls != 3 || len(full.ModelStats) != 1 || full.ModelStats[0].Model != "actual" {
		t.Fatal("overview cache contaminated full model statistics", err)
	}
	if fake.request.Include.ModelStats || fake.request.Include.EventsPage.Limit != actualModelPageSize {
		t.Fatal("full usage requested discarded upstream model statistics")
	}
	if _, err := k.GlobalUsage(t.Context(), now.Add(-7*24*time.Hour), now, 100); err != nil || fake.calls != 3 {
		t.Fatal("another preview size recomputed complete model statistics", err)
	}
	now = now.Add(k.CacheTTL + time.Second)
	if _, err := k.GlobalUsage(t.Context(), now.Add(-7*24*time.Hour), now, 0); err != nil || fake.calls != 4 {
		t.Fatal("expired full usage cache was retained", err)
	}
}

func TestAnalyticsRangeCacheKeepsScopeAndCalendarBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 59, 59, 0, time.UTC)
	span := 7 * 24 * time.Hour
	key := analyticsRangeKey(now, now.Add(-span), now)
	later := now.Add(2 * time.Second)
	if analyticsRangeKey(later, later.Add(-span), later) != key {
		t.Fatal("rolling cache expired at a minute boundary")
	}
	if analyticsRangeKey(now, now.Add(-24*time.Hour), now) == key {
		t.Fatal("different rolling ranges shared a cache")
	}
	if analyticsRangeKey(now, now.Add(-span-time.Nanosecond), now) == key {
		t.Fatal("hourly and daily statistics shared a cache")
	}
	fixedTo := now.Add(-24 * time.Hour)
	fixedKey := analyticsRangeKey(now, fixedTo.Add(-span), fixedTo)
	if analyticsRangeKey(later, fixedTo.Add(-span), fixedTo) != fixedKey || analyticsRangeKey(now, fixedTo.Add(-span), fixedTo.Add(time.Millisecond)) == fixedKey {
		t.Fatal("explicit historical boundaries were not preserved")
	}
	midnight := time.Date(2026, 10, 24, 15, 59, 59, 0, time.UTC)
	after := midnight.Add(2 * time.Second)
	if analyticsRangeKey(midnight, midnight.Add(-span), midnight) == analyticsRangeKey(after, after.Add(-span), after) {
		t.Fatal("rolling cache crossed the local date boundary")
	}
}

func TestAnalyticsQueriesShareWorkAndIsolateCancellation(t *testing.T) {
	k := NewKeys(nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(ctx context.Context) (cpamp.AnalyticsResponse, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-ctx.Done():
			return cpamp.AnalyticsResponse{}, ctx.Err()
		case <-release:
			return cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: 7}}, nil
		}
	}
	leader := make(chan error, 1)
	go func() { _, err := k.loadAnalytics(ctx, "shared", false, fetch); leader <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("shared query did not start")
	}
	cancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled page did not stop waiting")
	}
	var waiters sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			value, err := k.loadAnalytics(t.Context(), "shared", false, fetch)
			if err == nil && (value.Summary == nil || value.Summary.TotalCalls != 7) {
				err = errors.New("incorrect shared result")
			}
			results <- err
		}()
	}
	close(release)
	waiters.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("one page cancellation interrupted another", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent pages duplicated the upstream query")
	}
}

func TestAnalyticsErrorsAndManualRefreshDoNotReuseStaleResults(t *testing.T) {
	k := NewKeys(nil, nil)
	calls := 0
	fetch := func(context.Context) (cpamp.AnalyticsResponse, error) {
		calls++
		if calls == 1 {
			return cpamp.AnalyticsResponse{}, errors.New("temporary upstream failure")
		}
		return cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: int64(calls)}}, nil
	}
	if _, err := k.loadAnalytics(t.Context(), "refresh", false, fetch); err == nil {
		t.Fatal("upstream failure was hidden")
	}
	for _, refresh := range []bool{false, false, true, false} {
		value, err := k.loadAnalytics(t.Context(), "refresh", refresh, fetch)
		if err != nil || value.Summary.TotalCalls != int64(calls) {
			t.Fatal("cache/refresh returned an unexpected result", err)
		}
	}
	if calls != 3 {
		t.Fatal("failure cached or manual refresh did not replace the cached value")
	}
}
