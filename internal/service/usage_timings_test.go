package service

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/store"
)

func TestUsageTimingSharesCacheCutoffAndIncrementalReconciliation(t *testing.T) {
	k, _, now := modelHistoryFixture(t)
	path := filepath.Join(t.TempDir(), "portal.db")
	st, err := store.Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	maintenance, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	k.Store = st
	initial := *now
	save := func(id string, at time.Time, duration int64) {
		t.Helper()
		read, response := duration/4, duration/2
		if err := st.SaveGatewayRequestTiming(t.Context(), store.GatewayRequestTiming{ID: id, UserID: "a", StartedAt: at, TotalMS: duration, RequestReadMS: &read, ResponseStartedMS: &response}); err != nil {
			t.Fatal(err)
		}
	}
	save("expired", now.Add(-7*24*time.Hour+time.Second), 10)
	save("old", now.Add(-time.Hour), 1000)
	save("recent", now.Add(-time.Minute), 2000)
	first := queryModelHistory(t, k, *now, false)
	if first.TimingStats == nil || first.TimingStats.Samples != 3 || first.TimingStats.TotalMS != 3010 || first.TimingStats.ReconcileAtMS != first.ModelStatsReconcileAtMS {
		t.Fatal("local timings were not attached to the same usage result/deadline", first.TimingStats)
	}
	save("late", now.Add(-time.Hour+time.Second), 3000)
	save("next", now.Add(time.Second), 4000)
	*now = now.Add(time.Second)
	cached := queryModelHistory(t, k, *now, false)
	if cached.TimingStats != first.TimingStats || cached.WindowFromMS != first.WindowFromMS || cached.WindowToMS != first.WindowToMS {
		t.Fatal("cached page independently advanced timing statistics or its window")
	}
	*now = initial.Add(k.CacheTTL + time.Second)
	delta := queryModelHistory(t, k, *now, false)
	if delta.TimingStats.Samples != 4 || delta.TimingStats.TotalMS != 10000 || delta.TimingStats.ReconcileAtMS != first.TimingStats.ReconcileAtMS || delta.WindowToMS != now.UnixMilli() {
		t.Fatal("incremental timings lost a late sample or failed to expire the rolling lower edge", delta.TimingStats)
	}
	var samples, total, stage, parts int64
	for _, bucket := range delta.TimingStats.Buckets {
		samples += bucket.Samples
		total += bucket.TotalMS
		stage += bucket.StageSamples
		parts += bucket.UploadMS + bucket.WaitMS + bucket.ResponseMS
	}
	if samples != delta.TimingStats.Samples || total != delta.TimingStats.TotalMS || stage != samples || parts != total {
		t.Fatal("average and trend were not built from one complete sample cohort")
	}
	if _, err := maintenance.ExecContext(t.Context(), `UPDATE gateway_timing_samples SET total_ms=5000 WHERE id='old'`); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	manual := queryModelHistory(t, k, *now, true)
	if manual.TimingStats.TotalMS != 14000 || manual.TimingStats.ReconcileAtMS <= delta.TimingStats.ReconcileAtMS || manual.TimingStats.ReconcileAtMS != manual.ModelStatsReconcileAtMS {
		t.Fatal("manual model refresh did not also rebuild timings")
	}
	if _, err := maintenance.ExecContext(t.Context(), `UPDATE gateway_timing_samples SET total_ms=7000 WHERE id='old'`); err != nil {
		t.Fatal(err)
	}
	due := time.UnixMilli(manual.TimingStats.ReconcileAtMS)
	*now = due.Add(-time.Second)
	beforeDue := queryModelHistory(t, k, *now, false)
	if beforeDue.TimingStats.TotalMS != 14000 {
		t.Fatal("historical correction unexpectedly forced an early full scan")
	}
	*now = due
	periodic := queryModelHistory(t, k, *now, false)
	if periodic.TimingStats.TotalMS != 16000 || periodic.TimingStats.ReconcileAtMS <= due.UnixMilli() || periodic.TimingStats.ReconcileAtMS != periodic.ModelStatsReconcileAtMS {
		t.Fatal("page cache postponed the shared hourly full reconciliation")
	}
	if _, err := maintenance.ExecContext(t.Context(), `DELETE FROM gateway_timing_samples`); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(k.CacheTTL + time.Second)
	empty := queryModelHistory(t, k, *now, false)
	if empty.TimingStats.Note != "" || empty.TimingStats.Samples != 0 || empty.TimingStats.TotalMS != 0 {
		t.Fatal("rewritten sample table reused a checkpoint with a newer arrival cursor", empty.TimingStats)
	}
}

func TestUsageTimingScopesWeightingAndCheckpointBounds(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for i, user := range []string{"a", "a", "b"} {
		if err := st.SaveGatewayRequestTiming(t.Context(), store.GatewayRequestTiming{ID: user + string(rune('0'+i)), UserID: user, StartedAt: now.Add(-time.Duration(i+1) * time.Hour), TotalMS: int64(i+1) * 1000}); err != nil {
			t.Fatal(err)
		}
	}
	k := NewKeys(st, nil)
	k.Now = func() time.Time { return now }
	k.UsageTimingLocation = time.UTC
	req := cpamp.AnalyticsRequest{FromMS: now.Add(-7 * 24 * time.Hour).UnixMilli(), ToMS: now.UnixMilli(), Include: cpamp.AnalyticsInclude{Granularity: "hour"}}
	for user, want := range map[string]int64{"": 6000, "a": 3000, "b": 3000} {
		value := k.withUsageTimings(t.Context(), "scope:"+user, user, req, cpamp.AnalyticsResponse{}, false)
		if value.TimingStats == nil || value.TimingStats.TotalMS != want {
			t.Fatal("timings crossed global/user scope", user, value.TimingStats)
		}
		for _, bucket := range value.TimingStats.Buckets {
			if time.UnixMilli(bucket.BucketMS).UTC().Hour()%3 != 0 {
				t.Fatal("timing buckets diverged from the usage chart's timezone/granularity")
			}
		}
	}
	for i := 0; i <= actualModelMaxScopes; i++ {
		key := string(rune('A' + i))
		k.putUsageTimingSnapshot(key, &usageTimingSnapshot{updatedAt: now.Add(time.Duration(i) * time.Second), minutes: map[int64]store.GatewayTimingTotal{1: {Samples: 1}}})
	}
	if len(k.usageTimings) != actualModelMaxScopes || k.usageTimings["A"] != nil {
		t.Fatal("timing checkpoint count was not bounded")
	}
}

func TestFailedLocalTimingReadDoesNotPoisonAnalyticsCache(t *testing.T) {
	k, _, now := modelHistoryFixture(t)
	path := filepath.Join(t.TempDir(), "portal.db")
	st, err := store.Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	k.Store = st
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	value := queryModelHistory(t, k, *now, false)
	if value.TimingStats == nil || value.TimingStats.Note == "" {
		t.Fatal("timing query failure was hidden")
	}
	st, err = store.Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k.Store = st
	value = queryModelHistory(t, k, *now, false)
	if value.TimingStats.Note != "" || value.TimingStats.Samples != 0 {
		t.Fatal("timing failure was cached or an empty sample set fabricated data")
	}
}

func TestUsageTimingsRejectInconsistentStageTotals(t *testing.T) {
	if validTimingTotal(domain.GatewayTimingTotal{Samples: 1, TotalMS: 100, StageSamples: 1, UploadMS: 50, WaitMS: 30, ResponseMS: 30}) {
		t.Fatal("inconsistent stage totals accepted")
	}
}
