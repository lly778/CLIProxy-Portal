package store

import (
	"cliproxy-portal/internal/agy"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAGYUsageBoundariesAndKeyIsolation(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "portal.db"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	base := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	for i, second := range []int{0, 10, 60, 119, 120} {
		at := base.Add(time.Duration(second) * time.Second)
		row := GatewayRequestTiming{ID: time.Duration(i).String(), UserID: "owner", APIKeyHash: "key-a", CPARequestID: "agy:" + time.Duration(i).String(), StartedAt: at, EndedAt: at.Add(time.Second), TotalMS: 1000, Completed: true, StatusCode: 200, AGY: &AGYUsage{Model: "agy/model", Requested: "agy/model", Usage: agy.Usage{Known: true, Input: 1, Output: 2, Total: 3}}}
		if e = s.SaveGatewayRequestTiming(ctx, row); e != nil {
			t.Fatal(e)
		}
	}
	rows, e := s.AGYAggregates(ctx, base.Add(10*time.Second).UnixMilli(), base.Add(120*time.Second).UnixMilli(), []string{"key-a"})
	if e != nil {
		t.Fatal(e)
	}
	var calls, tokens int64
	for _, r := range rows {
		calls += r.Calls
		tokens += r.Total
	}
	if calls != 3 || tokens != 9 {
		t.Fatalf("boundary aggregation: %d calls %d tokens", calls, tokens)
	}
	rows, e = s.AGYAggregates(ctx, base.UnixMilli(), base.Add(3*time.Minute).UnixMilli(), []string{"key-a"}, "agy/unrelated")
	if e != nil || len(rows) != 0 {
		t.Fatal("model filter expanded to unrelated usage", e)
	}
	page, count, e := s.AGYRequestPage(ctx, base.UnixMilli(), base.Add(3*time.Minute).UnixMilli(), []string{"key-a"}, 100, "agy/unrelated")
	if e != nil || len(page) != 0 || count != 0 {
		t.Fatal("event model filter expanded", e)
	}
	rows, e = s.AGYAggregates(ctx, base.UnixMilli(), base.Add(3*time.Minute).UnixMilli(), []string{"key-b"})
	if e != nil || len(rows) != 0 {
		t.Fatal("partial key query leaked unrelated data")
	}
	var failures int
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM gateway_request_failures WHERE request_id LIKE 'agy:%'`).Scan(&failures); e != nil || failures != 0 {
		t.Fatal("AGY requests entered CPA failure reconciliation")
	}
}

func TestAGYReplayedResponseCountsTokensOncePerKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "portal.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Now().UTC().Truncate(time.Minute)
	for i, key := range []string{"key-a", "key-a", "key-b"} {
		at := base.Add(time.Duration(i) * time.Second)
		row := GatewayRequestTiming{ID: time.Duration(i).String(), UserID: "owner", APIKeyHash: key, StartedAt: at, EndedAt: at.Add(time.Second), TotalMS: 1000, Completed: true, StatusCode: 200,
			AGY: &AGYUsage{Model: "agy/model", Requested: "agy/model", ResponseID: "resp-original", Usage: agy.Usage{Known: true, Input: 100, Output: 20, Total: 120}}}
		if err := s.SaveGatewayRequestTiming(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"key-a", "key-b"} {
		rows, err := s.AGYAggregates(t.Context(), base.UnixMilli(), base.Add(time.Minute).UnixMilli(), []string{key})
		if err != nil || len(rows) != 1 || rows[0].Total != 120 {
			t.Fatalf("replayed response duplicated or crossed key usage: %s %+v %v", key, rows, err)
		}
		wantCalls := int64(1)
		if key == "key-a" {
			wantCalls = 2
		}
		if rows[0].Calls != wantCalls {
			t.Fatal("HTTP attempt count changed while deduplicating model tokens")
		}
	}
}
