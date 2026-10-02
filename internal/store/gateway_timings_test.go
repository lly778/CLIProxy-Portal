package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTimingRetentionDoesNotDependOnDialogueRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	start := time.Now().Add(-8 * 24 * time.Hour)
	for i := 0; i < 101; i++ {
		row := GatewayRequestTiming{ID: fmt.Sprintf("timing-%03d", i), UserID: "a", APIKeyHash: "hash-a", CPARequestID: fmt.Sprintf("request-%03d", i), StartedAt: start.Add(time.Duration(i) * time.Second), EndedAt: start.Add(time.Duration(i+1) * time.Second), TotalMS: 1000, Completed: true, StartSource: "connection_accepted"}
		if err := s.SaveGatewayRequestTiming(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GatewayTimingByCPARequest(ctx, "request-000", "hash-a"); err != ErrNotFound {
		t.Fatalf("oldest timing not pruned: %v", err)
	}
	row, err := s.GatewayTimingByCPARequest(ctx, "request-001", "hash-a")
	if err != nil || row.TotalMS != 1000 || row.RequestReadMS != nil || !row.Completed {
		t.Fatalf("retained timing/null stages = %+v, %v", row, err)
	}
	if _, err := s.GatewayTimingByCPARequest(ctx, "request-001", "hash-b"); err != ErrNotFound {
		t.Fatalf("cross-key timing lookup: %v", err)
	}
	total, err := s.GatewayTimingSummary(ctx, start, start.Add(time.Hour), "a")
	if err != nil || total.Samples != 101 || total.TotalMS != 101000 {
		t.Fatalf("trend lost pruned detail: %+v, %v", total, err)
	}
	if err := s.migrate(ctx, true); err != nil {
		t.Fatal(err)
	}
	total, err = s.GatewayTimingSummary(ctx, start, start.Add(time.Hour), "a")
	if err != nil || total.Samples != 101 {
		t.Fatalf("migration duplicated samples: %+v, %v", total, err)
	}
}

func TestTimingTotalsExactBoundariesUserFilterAndWeightedBuckets(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	anchor := time.Date(2026, 10, 2, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	from, to := anchor.Add(15*time.Minute), anchor.Add(4*time.Hour+15*time.Minute)
	for i, row := range []struct {
		at    time.Time
		user  string
		total int64
	}{
		{from.Add(-time.Millisecond), "a", 99999},
		{from, "a", 1000}, {anchor.Add(time.Hour), "a", 9000}, {anchor.Add(2 * time.Hour), "a", 2000},
		{anchor.Add(3 * time.Hour), "a", 8000}, {to, "a", 99999},
		{anchor.Add(2 * time.Hour), "b", 30000},
	} {
		if err := s.SaveGatewayRequestTiming(ctx, GatewayRequestTiming{ID: fmt.Sprintf("range-%d", i), UserID: row.user, StartedAt: row.at, TotalMS: row.total, Completed: i%2 == 0}); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := s.GatewayTimingTotals(ctx, from, to, "a", anchor, 3*time.Hour)
	if err != nil || len(totals) != 2 || totals[0].Samples != 3 || totals[0].TotalMS != 12000 || totals[1].Samples != 1 || totals[1].TotalMS != 8000 {
		t.Fatalf("three-hour totals = %+v, %v", totals, err)
	}
	summary, err := s.GatewayTimingSummary(ctx, from, to, "")
	if err != nil || summary.Samples != 5 || summary.TotalMS != 50000 {
		t.Fatalf("global summary = %+v, %v", summary, err)
	}
	if _, err := s.GatewayTimingTotals(ctx, from, to, "a", anchor, 0); err == nil {
		t.Fatal("zero bucket must fail")
	}
}

func TestTimingSamplesSeedLegacyDetailsOnce(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	start := time.Now().UTC()
	if err := s.SaveGatewayRequestTiming(ctx, GatewayRequestTiming{ID: "legacy", UserID: "a", StartedAt: start, TotalMS: 5000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_timing_samples`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.migrate(ctx, true); err != nil {
			t.Fatal(err)
		}
		summary, err := s.GatewayTimingSummary(ctx, start, start.Add(time.Hour), "a")
		if err != nil || summary.Samples != 1 || summary.TotalMS != 5000 {
			t.Fatalf("legacy seed = %+v, %v", summary, err)
		}
	}
}
