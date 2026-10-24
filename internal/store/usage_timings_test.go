package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsageTimingDeltaFindsLateCompletionsAndUsesExactWindow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	from := time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	save := func(id, user string, at time.Time, duration int64) {
		t.Helper()
		read, response := duration/4, duration/2
		if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: id, UserID: user, StartedAt: at, TotalMS: duration, RequestReadMS: &read, ResponseStartedMS: &response}); err != nil {
			t.Fatal(err)
		}
	}
	save("outside", "a", from.Add(-time.Millisecond), 99999)
	save("expired", "a", from.Add(10*time.Second), 10)
	save("untouched", "a", from.Add(48*time.Hour), 7000)
	save("middle", "a", from.Add(72*time.Hour), 1000)
	save("recent", "a", to.Add(-time.Minute), 2000)
	save("boundary", "a", to, 5000)
	save("future", "a", to.Add(time.Minute), 3000)
	save("foreign", "b", from.Add(72*time.Hour), 99999)
	full, err := s.ReadUsageTimings(t.Context(), from.UnixMilli(), to.UnixMilli(), "a", nil, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	totals := func(batch TimingReadBatch) GatewayTimingTotal {
		var sum GatewayTimingTotal
		for _, row := range batch.Totals {
			sum.Samples += row.Samples
			sum.TotalMS += row.TotalMS
			sum.StageSamples += row.StageSamples
			sum.UploadMS += row.UploadMS
			sum.WaitMS += row.WaitMS
			sum.ResponseMS += row.ResponseMS
		}
		return sum
	}
	if sum := totals(full); sum.Samples != 4 || sum.TotalMS != 10010 {
		t.Fatal("full query included a foreign user or outside-boundary sample", sum)
	}
	// This request finished late: its start is three days behind the cursor,
	// well outside the recent overlap. Its entire minute must be recomputed.
	save("late-completion", "a", from.Add(72*time.Hour+time.Millisecond), 4000)
	save("new", "a", to.Add(30*time.Second), 5000)
	if _, err := s.db.Exec(`UPDATE gateway_timing_samples SET total_ms=3000 WHERE id='recent'`); err != nil {
		t.Fatal(err)
	}
	nextFrom, nextTo := from.Add(2*time.Minute), to.Add(2*time.Minute)
	delta, err := s.ReadUsageTimings(t.Context(), nextFrom.UnixMilli(), nextTo.UnixMilli(), "a", &TimingReadWindow{FromMS: from.UnixMilli(), ToMS: to.UnixMilli(), Cursor: full.Cursor}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if sum := totals(delta); sum.Samples != 6 || sum.TotalMS != 21000 || sum.StageSamples != 6 || sum.UploadMS+sum.WaitMS+sum.ResponseMS != sum.TotalMS {
		t.Fatal("delta lost a late completion, boundary, stage correction, or duplicated overlap", sum)
	}
	for _, row := range delta.Totals {
		if row.BucketMS == from.Add(48*time.Hour).UnixMilli()/TimingSampleBucketMS*TimingSampleBucketMS {
			t.Fatal("delta returned unchanged historical buckets")
		}
	}
	if delta.Cursor <= full.Cursor {
		t.Fatal("arrival cursor failed to advance")
	}
	check, err := s.ReadUsageTimings(t.Context(), nextFrom.UnixMilli(), nextTo.UnixMilli(), "a", nil, 5*time.Minute)
	if err != nil || totals(check).TotalMS != totals(delta).TotalMS+7000 || totals(check).Samples != totals(delta).Samples+1 {
		t.Fatal("delta plus unchanged history disagrees with a fresh full query", err)
	}
	global, err := s.ReadUsageTimings(t.Context(), nextFrom.UnixMilli(), nextTo.UnixMilli(), "", nil, 5*time.Minute)
	if err != nil || totals(global).TotalMS != totals(check).TotalMS+99999 {
		t.Fatal("global and per-user cohorts were mixed", err)
	}
}

func TestUsageTimingDeltaRemovesEmptyRecentBuckets(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	from := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: "recent", UserID: "a", StartedAt: from.Add(59 * time.Minute), TotalMS: 1000}); err != nil {
		t.Fatal(err)
	}
	to := from.Add(time.Hour)
	full, err := s.ReadUsageTimings(t.Context(), from.UnixMilli(), to.UnixMilli(), "a", nil, 5*time.Minute)
	if err != nil || len(full.Totals) != 1 {
		t.Fatal("missing baseline", err)
	}
	if _, err := s.db.Exec(`DELETE FROM gateway_timing_samples`); err != nil {
		t.Fatal(err)
	}
	delta, err := s.ReadUsageTimings(t.Context(), from.UnixMilli(), to.UnixMilli(), "a", &TimingReadWindow{FromMS: from.UnixMilli(), ToMS: to.UnixMilli(), Cursor: full.Cursor}, 5*time.Minute)
	if err != nil || len(delta.Totals) != 0 || delta.Cursor >= full.Cursor {
		t.Fatal("empty/deleted table did not expose its invalidated cursor", err, delta)
	}
}
