package service

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

func sourceRequest(now time.Time) cpamp.AnalyticsRequest {
	return cpamp.AnalyticsRequest{FromMS: now.Add(-7 * 24 * time.Hour).UnixMilli(), ToMS: now.UnixMilli(), NowMS: now.UnixMilli(), TimeZone: "Asia/Shanghai", Include: cpamp.AnalyticsInclude{Granularity: "hour"}}
}

func TestUsageSourceReusesSupersetWithoutExpandingColdPartialReads(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fake := &userRequestsCPAMP{value: cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: 8}, Timeline: []cpamp.UsageTimelinePoint{{Calls: 8}}, APIKeyStats: []cpamp.APIKeyUsageStat{{APIKeyHash: "a", Calls: 3}, {APIKeyHash: "b", Calls: 5}}, Events: &cpamp.EventsResponse{}}}
	k := NewKeys(nil, fake)
	k.Now = func() time.Time { return now }
	req := sourceRequest(now)
	req.Include.Summary, req.Include.Timeline, req.Include.APIKeyStats = true, true, true
	if _, err := k.queryUsageAnalytics(t.Context(), req, false); err != nil {
		t.Fatal(err)
	}
	partial := sourceRequest(now)
	partial.Include.APIKeyStats = true
	partial.Filters.APIKeyHashes = []string{"b"}
	value, err := k.queryUsageAnalytics(t.Context(), partial, false)
	if err != nil || len(fake.requests) != 1 || len(value.APIKeyStats) != 1 || value.APIKeyStats[0].APIKeyHash != "b" || value.Summary != nil || value.Timeline != nil || value.Events != nil {
		t.Fatalf("subset did not reuse/project full data: %+v, %v, calls=%d", value, err, len(fake.requests))
	}
	value.APIKeyStats[0].Calls = 999
	value, _ = k.queryUsageAnalytics(t.Context(), partial, false)
	if value.APIKeyStats[0].Calls != 5 {
		t.Fatal("caller mutated cached source")
	}
	k = NewKeys(nil, fake)
	k.Now = func() time.Time { return now }
	fake.requests = nil
	if _, err := k.queryUsageAnalytics(t.Context(), partial, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 1 || !reflect.DeepEqual(fake.requests[0], partial) {
		t.Fatalf("cold partial query expanded: %+v", fake.requests)
	}
}

func TestUsageSourceSeparatesUsersFiltersRangesAndLogCursors(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fake := &userRequestsCPAMP{value: cpamp.AnalyticsResponse{Events: &cpamp.EventsResponse{}}}
	k := NewKeys(nil, fake)
	k.Now = func() time.Time { return now }
	req := sourceRequest(now)
	req.Include.EventsPage = &cpamp.EventsPage{Limit: 8}
	query := func(req cpamp.AnalyticsRequest) {
		t.Helper()
		if _, err := k.queryUsageAnalytics(t.Context(), req, false); err != nil {
			t.Fatal(err)
		}
	}
	query(req)
	query(req)
	personal := req
	personal.Filters.APIKeyHashes = []string{"a"}
	query(personal)
	filtered := req
	filtered.Filters.Models = []string{"model"}
	query(filtered)
	historical := req
	historical.FromMS -= 1
	query(historical)
	large := req
	large.Include.EventsPage = &cpamp.EventsPage{Limit: 100}
	query(large)
	before := now.UnixMilli() - 1000
	cursor := req
	cursor.Include.EventsPage = &cpamp.EventsPage{Limit: 8, BeforeMS: &before}
	query(cursor)
	query(cursor)
	if len(fake.requests) != 7 {
		t.Fatalf("scope/cursor isolation lost: %+v", fake.requests)
	}
}

type usageSourceBlockingCPAMP struct {
	cpamp.API
	reads            atomic.Int32
	started, release chan struct{}
}

func (f *usageSourceBlockingCPAMP) Analytics(ctx context.Context, _ cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	count := f.reads.Add(1)
	if count == 1 {
		close(f.started)
		select {
		case <-ctx.Done():
			return cpamp.AnalyticsResponse{}, ctx.Err()
		case <-f.release:
		}
	}
	return cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: int64(count)}}, nil
}

func TestUsageSourceCancellationAndRefreshFence(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fake := &usageSourceBlockingCPAMP{started: make(chan struct{}), release: make(chan struct{})}
	k := NewKeys(nil, fake)
	k.Now = func() time.Time { return now }
	req := sourceRequest(now)
	req.Include.Summary = true
	waiter, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { _, err := k.queryUsageAnalytics(waiter, req, false); finished <- err }()
	<-fake.started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation=%v", err)
	}
	value, err := k.queryUsageAnalytics(t.Context(), req, true)
	if err != nil || value.Summary.TotalCalls != 2 {
		t.Fatalf("refresh reused old flight: %+v %v", value, err)
	}
	close(fake.release)
	value, err = k.queryUsageAnalytics(t.Context(), req, false)
	if err != nil || value.Summary.TotalCalls != 2 {
		t.Fatal("old flight overwrote refreshed source")
	}
}

func TestUsageSourceDoesNotRetainFullHistoryOrExtendOriginalExpiry(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fake := &userRequestsCPAMP{value: cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: 1}, Events: &cpamp.EventsResponse{}}}
	k := NewKeys(nil, fake)
	k.Now = func() time.Time { return now }
	req := sourceRequest(now)
	req.Include.Summary = true
	req.Include.EventsPage = &cpamp.EventsPage{Limit: 5000}
	if _, err := k.queryUsageAnalytics(t.Context(), req, false); err != nil {
		t.Fatal(err)
	}
	partial := req
	partial.Include.EventsPage = nil
	now = now.Add(time.Minute)
	value, err := k.queryUsageAnalytics(t.Context(), partial, false)
	if err != nil || value.Events != nil || len(fake.requests) != 1 {
		t.Fatal("history retained or aggregate not reused")
	}
	k.putCache("derived", value)
	if !k.cache["derived"].expiresAt.Equal(time.UnixMilli(value.SourceExpiresAtMS)) {
		t.Fatal("derived cache extended original expiry")
	}
	if _, err := k.queryUsageAnalytics(t.Context(), req, false); err != nil || len(fake.requests) != 2 {
		t.Fatal("full history page retained in source cache")
	}
}
