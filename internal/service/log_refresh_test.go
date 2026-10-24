package service

import (
	"context"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

type logRefreshCPAMP struct {
	cpamp.API
	calls int
}

func (f *logRefreshCPAMP) Analytics(_ context.Context, _ cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	f.calls++
	return cpamp.AnalyticsResponse{Events: &cpamp.EventsResponse{TotalCount: int64(f.calls), Items: []cpamp.EventRow{{TimestampMS: int64(f.calls)}}}}, nil
}

func TestManualGlobalLogRefreshBypassesAndReplacesCache(t *testing.T) {
	fake := &logRefreshCPAMP{}
	k := NewKeys(nil, fake)
	now := time.Date(2026, 9, 23, 12, 0, 59, 0, time.UTC)
	k.Now = func() time.Time { return now }
	first, err := k.GlobalRequests(context.Background(), 100)
	if err != nil || first.TotalCount != 1 {
		t.Fatalf("first request = %+v, %v", first, err)
	}
	now = now.Add(2 * time.Second)
	cached, err := k.GlobalRequests(context.Background(), 100)
	if err != nil || cached.TotalCount != 1 || fake.calls != 1 {
		t.Fatalf("ordinary reload bypassed cache: result=%+v calls=%d err=%v", cached, fake.calls, err)
	}
	refreshed, err := k.RefreshGlobalRequests(context.Background(), 100)
	if err != nil || refreshed.TotalCount != 2 || fake.calls != 2 {
		t.Fatalf("manual refresh did not fetch fresh events: result=%+v calls=%d err=%v", refreshed, fake.calls, err)
	}
	cached, err = k.GlobalRequests(context.Background(), 100)
	if err != nil || cached.TotalCount != 2 || fake.calls != 2 {
		t.Fatalf("ordinary reload did not use refreshed cache: result=%+v calls=%d err=%v", cached, fake.calls, err)
	}
	now = now.Add(k.CacheTTL + time.Second)
	if _, err := k.GlobalRequests(context.Background(), 100); err != nil || fake.calls != 3 {
		t.Fatal("expired global request cache was retained", err)
	}
}
