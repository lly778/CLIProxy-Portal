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
		read, response := row.total/4, row.total/2
		if err := s.SaveGatewayRequestTiming(ctx, GatewayRequestTiming{ID: fmt.Sprintf("range-%d", i), UserID: row.user, StartedAt: row.at, TotalMS: row.total, RequestReadMS: &read, ResponseStartedMS: &response, Completed: i%2 == 0}); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := s.GatewayTimingTotals(ctx, from, to, "a", anchor, 3*time.Hour)
	if err != nil || len(totals) != 2 || totals[0].Samples != 3 || totals[0].TotalMS != 12000 || totals[1].Samples != 1 || totals[1].TotalMS != 8000 {
		t.Fatalf("three-hour totals = %+v, %v", totals, err)
	}
	if totals[0].StageSamples != 3 || totals[0].UploadMS != 3000 || totals[0].WaitMS != 3000 || totals[0].ResponseMS != 6000 || totals[1].UploadMS != 2000 || totals[1].WaitMS != 2000 || totals[1].ResponseMS != 4000 {
		t.Fatalf("stage range/grouping differs from total = %+v", totals)
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

func TestTimingStageSamplesSurviveDetailPruning(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	read, response := int64(100), int64(400)
	for i := 0; i < 101; i++ {
		if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: fmt.Sprintf("stage-%d", i), UserID: "a", StartedAt: start.Add(time.Duration(i) * time.Second), TotalMS: 1000, RequestReadMS: &read, ResponseStartedMS: &response, Completed: i%2 == 0}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.migrate(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		totals, err := s.GatewayTimingTotals(t.Context(), start, start.Add(time.Hour), "a", start, time.Hour)
		if err != nil || len(totals) != 1 {
			t.Fatalf("totals = %+v, %v", totals, err)
		}
		got := totals[0]
		if got.Samples != 101 || got.StageSamples != 101 || got.UploadMS != 10100 || got.WaitMS != 30300 || got.ResponseMS != 60600 || got.TotalMS != 101000 {
			t.Fatalf("pruned/restarted stages = %+v", got)
		}
	}
}

func TestTimingStagesExcludeMissingAndInconsistentBoundaries(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	ptr := func(v int64) *int64 { return &v }
	for i, row := range []GatewayRequestTiming{
		{TotalMS: 1000, RequestReadMS: ptr(100), ResponseStartedMS: ptr(300)},
		{TotalMS: 1000},
		{TotalMS: 1000, RequestReadMS: ptr(100)},
		{TotalMS: 1000, RequestReadMS: ptr(-1), ResponseStartedMS: ptr(300)},
		{TotalMS: 1000, RequestReadMS: ptr(400), ResponseStartedMS: ptr(300)},
		{TotalMS: 1000, RequestReadMS: ptr(100), ResponseStartedMS: ptr(1100)},
		{TotalMS: 0, RequestReadMS: ptr(0), ResponseStartedMS: ptr(0)},
	} {
		row.ID, row.UserID, row.StartedAt = fmt.Sprintf("invalid-%d", i), "a", start
		if err := s.SaveGatewayRequestTiming(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: "other", UserID: "b", StartedAt: start, TotalMS: 9999, RequestReadMS: ptr(1), ResponseStartedMS: ptr(2)}); err != nil {
		t.Fatal(err)
	}
	totals, err := s.GatewayTimingTotals(t.Context(), start, start.Add(time.Hour), "a", start, time.Hour)
	if err != nil || len(totals) != 1 {
		t.Fatalf("totals = %+v, %v", totals, err)
	}
	got := totals[0]
	if got.Samples != 7 || got.StageSamples != 2 || got.TotalMS != 6000 || got.UploadMS != 100 || got.WaitMS != 200 || got.ResponseMS != 700 {
		t.Fatalf("invalid stages treated as zero/valid = %+v", got)
	}
}

func TestTimingStageMigrationFromTotalOnlySchema(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	read, response := int64(200), int64(800)
	if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: "retained", UserID: "a", StartedAt: start, TotalMS: 1000, RequestReadMS: &read, ResponseStartedMS: &response}); err != nil {
		t.Fatal(err)
	}
	// Recreate the previous schema only in this isolated test database.
	if _, err := s.db.Exec(`DROP TABLE gateway_timing_samples;
		CREATE TABLE gateway_timing_samples(id TEXT PRIMARY KEY,user_id TEXT NOT NULL,started_at_ms INTEGER NOT NULL,total_ms INTEGER NOT NULL CHECK(total_ms>=0))`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"retained", "pruned"} {
		if _, err := s.db.Exec(`INSERT INTO gateway_timing_samples VALUES(?,?,?,?)`, id, "a", start.UnixMilli(), 1000); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.migrate(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		totals, err := s.GatewayTimingTotals(t.Context(), start, start.Add(time.Hour), "a", start, time.Hour)
		if err != nil || len(totals) != 1 {
			t.Fatalf("totals = %+v, %v", totals, err)
		}
		got := totals[0]
		if got.Samples != 2 || got.StageSamples != 1 || got.TotalMS != 2000 || got.UploadMS != 200 || got.WaitMS != 600 || got.ResponseMS != 200 {
			t.Fatalf("migration fabricated/lost history = %+v", got)
		}
	}
}
