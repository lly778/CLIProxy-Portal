package store

import (
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
)

func TestAGYObservationLedgerDedupIsolationAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute)
	v := agy.Observation{ID: "summary-one", Owner: "owner:key-a", RequestID: "request", Model: "agy/test", Kind: "summary_usage", State: "completed", At: base.Add(10 * time.Second).UnixMilli(), Usage: agy.Usage{Known: true, Input: 100, Output: 20, Cache: 30, Reasoning: 5, Total: 120}}
	for i := 0; i < 2; i++ {
		if err = s.SaveAGYObservation(t.Context(), v); err != nil {
			t.Fatal(err)
		}
	}
	v.Owner = "other:key-b"
	if err = s.SaveAGYObservation(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	v.Owner, v.ID, v.State, v.At = "owner:key-a", "failed-summary", "failed", base.Add(30*time.Second).UnixMilli()
	if err = s.SaveAGYObservation(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: "request", UserID: "owner", APIKeyHash: "key-a", StartedAt: base.Add(20 * time.Second), EndedAt: base.Add(21 * time.Second), TotalMS: 1000, Completed: true, StatusCode: 200, AGY: &AGYUsage{Model: "agy/test", Usage: agy.Usage{Known: true, Input: 200, Output: 10, Total: 210}}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.AGYObservations(t.Context(), "request", "key-a")
	if err != nil || len(events) != 2 {
		t.Fatal("receipts lost or duplicated across restart", events, err)
	}
	events, err = s.AGYObservations(t.Context(), "request", "key-c")
	if err != nil || len(events) != 0 {
		t.Fatal("cross-key receipt leak", err)
	}
	check := func(from, to int64, key string, wantCalls, wantTokens, wantUnknown int64, models ...string) {
		t.Helper()
		rows, err := s.AGYAggregates(t.Context(), from, to, []string{key}, models...)
		if err != nil {
			t.Fatal(err)
		}
		var calls, tokens, unknown int64
		for _, r := range rows {
			calls += r.Calls
			tokens += r.Total
			unknown += r.Unknown
		}
		if calls != wantCalls || tokens != wantTokens || unknown != wantUnknown {
			t.Fatalf("calls=%d tokens=%d unknown=%d rows=%+v", calls, tokens, unknown, rows)
		}
	}
	check(base.UnixMilli(), base.Add(time.Minute).UnixMilli(), "key-a", 1, 450, 1)
	check(base.Add(11*time.Second).UnixMilli(), base.Add(30*time.Second).UnixMilli(), "key-a", 1, 210, 0)
	check(base.UnixMilli(), base.Add(time.Minute).UnixMilli(), "key-b", 0, 120, 0)
	check(base.UnixMilli(), base.Add(time.Minute).UnixMilli(), "key-c", 0, 0, 0)
	check(base.UnixMilli(), base.Add(time.Minute).UnixMilli(), "key-a", 0, 0, 0, "agy/another")
	if _, err = s.db.Exec(`UPDATE agy_observations SET at_ms=1`); err != nil {
		t.Fatal(err)
	}
	if err = s.PruneAGYObservations(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err = s.AGYObservations(t.Context(), "request", "key-a")
	if err != nil || len(events) != 0 {
		t.Fatal("receipt retention not applied", err)
	}
	check(base.UnixMilli(), base.Add(time.Minute).UnixMilli(), "key-a", 1, 450, 1)
	// Pruning detail must not permit the same native cost to be billed twice.
	if err = s.SaveAGYObservation(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	check(base.UnixMilli(), base.Add(time.Minute).UnixMilli(), "key-a", 1, 450, 1)
}
