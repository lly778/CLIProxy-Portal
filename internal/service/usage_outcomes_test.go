package service

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/store"
)

func TestUsageOutcomeCheckpointExpiresReplacesAndReconciles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	st, err := store.Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k := NewKeys(st, nil)
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	initial := now
	k.Now = func() time.Time { return now }
	from := now.Add(-7 * 24 * time.Hour)
	save := func(id string, at time.Time) {
		t.Helper()
		if err := st.SaveGatewayRequestTiming(t.Context(), store.GatewayRequestTiming{ID: id, UserID: "a", APIKeyHash: "key", StartedAt: at, EndedAt: at.Add(time.Second), TotalMS: 1000, StatusCode: 502, FailureReason: "preparation_failed"}); err != nil {
			t.Fatal(err)
		}
	}
	save("expires", from.Add(time.Second))
	save("old", now.Add(-3*24*time.Hour))
	save("recent", now.Add(-time.Minute))
	if err := st.ResourceAuditBatch(t.Context(), []domain.AuditEvent{{Action: "resource.rejected", TargetID: "a", CreatedAt: from.Add(10 * time.Second)}, {Action: "resource.rejected", TargetID: "a", CreatedAt: now.Add(-3 * 24 * time.Hour)}, {Action: "resource.observed", TargetID: "a", CreatedAt: now.Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	read := func(refresh bool) *domain.RequestLogData {
		t.Helper()
		req := cpamp.AnalyticsRequest{FromMS: now.Add(-7 * 24 * time.Hour).UnixMilli(), ToMS: now.UnixMilli(), Include: cpamp.AnalyticsInclude{Granularity: "hour"}}
		value, err := k.requestLogSource(t.Context(), []string{"a"}, false, req, refresh)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	count := func(d *domain.RequestLogData) int64 {
		var n int64
		for _, r := range d.Corrections {
			n += r.Calls
			if r.Failure != r.Calls || r.Success != 0 {
				t.Fatal("invalid standalone failure correction")
			}
		}
		return n
	}
	first := read(false)
	if count(first) != 5 {
		t.Fatal("initial failures not complete", first)
	}
	save("late", now.Add(-2*24*time.Hour))
	save("new", now.Add(time.Minute))
	if err := st.ResourceAuditBatch(t.Context(), []domain.AuditEvent{{Action: "resource.rejected", TargetID: "a", CreatedAt: now.Add(-2 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	now = initial.Add(2 * time.Minute)
	next := read(false)
	if count(next) != 6 || count(first) != 5 || next.ReconcileAtMS != first.ReconcileAtMS {
		t.Fatal("late arrival, expiry, immutability or full deadline lost", first, next)
	}
	maintenance, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	if _, err = maintenance.Exec(`DELETE FROM gateway_request_failures WHERE id='old'`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if count(read(false)) != 6 {
		t.Fatal("historical rows were recounted instead of delta read")
	}
	manual := read(true)
	if count(manual) != 5 || manual.ReconcileAtMS <= next.ReconcileAtMS {
		t.Fatal("manual refresh did not rebuild the same outcome scope")
	}
	now = time.UnixMilli(manual.ReconcileAtMS)
	periodic := read(false)
	if count(periodic) != 5 || periodic.ReconcileAtMS <= manual.ReconcileAtMS {
		t.Fatal("hourly full reconciliation was postponed")
	}
}

func TestUsageOutcomeSharesUsageCacheWindowAndDeadline(t *testing.T) {
	_, st := accountsForTest(t)
	k := NewKeys(st, nil)
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	initial := now
	k.Now = func() time.Time { return now }
	save := func(id string) {
		t.Helper()
		if err := st.SaveGatewayRequestTiming(t.Context(), store.GatewayRequestTiming{ID: id, UserID: "a", StartedAt: initial.Add(-time.Hour), EndedAt: initial.Add(-time.Hour + time.Second), TotalMS: 1000, StatusCode: 502, FailureReason: "preparation_failed"}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() cpamp.AnalyticsResponse {
		t.Helper()
		a, err := k.UserOverviewUsage(t.Context(), "a", now.Add(-24*time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	save("one")
	first := read()
	save("two")
	now = now.Add(time.Second)
	cached := read()
	if first.Summary.TotalCalls != 1 || cached.Summary.TotalCalls != 1 || cached.RequestLogData != first.RequestLogData || cached.WindowToMS != first.WindowToMS {
		t.Fatal("outcomes independently advanced a cached usage result")
	}
	now = initial.Add(k.CacheTTL + time.Second)
	delta := read()
	if delta.Summary.TotalCalls != 2 || delta.Summary.FailureCalls != 2 || delta.RequestLogData.ReconcileAtMS != first.RequestLogData.ReconcileAtMS {
		t.Fatal("usage cache miss did not include late failure")
	}
	now = time.UnixMilli(first.RequestLogData.ReconcileAtMS).Add(-time.Second)
	_ = read()
	now = now.Add(time.Second)
	due := read()
	if due.RequestLogData.ReconcileAtMS <= first.RequestLogData.ReconcileAtMS {
		t.Fatal("two-minute usage cache postponed hourly full reconciliation")
	}
}

type statusModelCPAMP struct{ *incrementalModelCPAMP }

func (f statusModelCPAMP) Analytics(ctx context.Context, req cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	a, err := f.incrementalModelCPAMP.Analytics(ctx, req)
	if a.Summary != nil {
		for _, r := range f.rows {
			if r.TimestampMS >= req.FromMS && r.TimestampMS < req.ToMS {
				if r.Failed {
					a.Summary.FailureCalls++
				} else {
					a.Summary.SuccessCalls++
				}
			}
		}
	}
	return a, err
}

func TestStatusOnlyCorrectionInvalidatesModelCheckpoint(t *testing.T) {
	k, f, now := modelHistoryFixture(t)
	k.CPAMP = statusModelCPAMP{f}
	// Keep the selected range fixed while expiring the cache. Advancing a
	// rolling seven-day range would also remove a row near its old boundary.
	from, to := now.Add(-7*24*time.Hour), *now
	read := func() cpamp.AnalyticsResponse {
		t.Helper()
		value, err := k.GlobalUsage(t.Context(), from, to, 0)
		if err != nil || value.ModelStatsNote != "" {
			t.Fatal("complete model statistics unavailable", err, value.ModelStatsNote)
		}
		return value
	}
	first := read()
	f.rows[10].Failed = true
	*now = now.Add(k.CacheTTL + time.Second)
	next := read()
	if next.Summary.TotalCalls != first.Summary.TotalCalls || next.Summary.TotalTokens != first.Summary.TotalTokens || next.Summary.FailureCalls != 1 || next.Summary.SuccessCalls+next.Summary.FailureCalls != next.Summary.TotalCalls {
		t.Fatal("status-only correction left stale incremental success/failure counters")
	}
}

func TestUsageOutcomeReclassifiesLateUpstreamWithoutDuplicateCalls(t *testing.T) {
	_, st := accountsForTest(t)
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	at := now.Add(-2 * time.Hour)
	for _, id := range []string{"one", "duplicate"} {
		if err := st.SaveGatewayRequestTiming(t.Context(), store.GatewayRequestTiming{ID: id, UserID: "a", APIKeyHash: "key", CPARequestID: "request", StartedAt: at, EndedAt: at.Add(time.Minute), TotalMS: 60000, StatusCode: 200, FailureReason: "idle_timeout"}); err != nil {
			t.Fatal(err)
		}
	}
	fake := &failureReconcileCPAMP{}
	k := NewKeys(st, fake)
	k.Now = func() time.Time { return now }
	read := func(refresh bool) *domain.RequestLogData {
		t.Helper()
		req := cpamp.AnalyticsRequest{FromMS: now.Add(-7 * 24 * time.Hour).UnixMilli(), ToMS: now.UnixMilli(), Filters: cpamp.AnalyticsFilters{APIKeyHashes: []string{"key"}}, Include: cpamp.AnalyticsInclude{Granularity: "hour"}}
		d, err := k.requestLogSource(t.Context(), []string{"a"}, false, req, refresh)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	first := read(false)
	if len(first.Corrections) != 1 || first.Corrections[0].Calls != 1 || first.Corrections[0].Failure != 1 {
		t.Fatal("unmatched trace was counted more than once")
	}
	now = now.Add(2 * time.Minute)
	fake.events = []cpamp.EventRow{{EventHash: "event", RequestID: "request", APIKeyHash: "key", TimestampMS: at.Add(time.Second).UnixMilli()}}
	matched := read(false)
	if len(matched.Corrections) != 1 || matched.Corrections[0].Calls != 0 || matched.Corrections[0].Success != -1 || matched.Corrections[0].Failure != 1 {
		t.Fatal("late upstream did not replace standalone failure", matched)
	}
	fake.events[0].Failed = true
	alreadyFailed := read(true)
	if len(alreadyFailed.Corrections) != 0 || len(first.Corrections) != 1 || first.Corrections[0].Calls != 1 {
		t.Fatal("upstream failure duplicated count or mutated older cached response")
	}
}
