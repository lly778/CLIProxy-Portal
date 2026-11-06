package store

import (
	"cliproxy-portal/internal/domain"
	"testing"
	"time"
)

func TestUsageOutcomesReadsDeltaAndLateArrivalsWithinUserScope(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	from := now.Add(-7 * 24 * time.Hour)
	save := func(id, user string, at time.Time) {
		t.Helper()
		if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: id, UserID: user, APIKeyHash: "key", StartedAt: at, EndedAt: at.Add(time.Second), TotalMS: 1000, StatusCode: 502, FailureReason: "preparation_failed"}); err != nil {
			t.Fatal(err)
		}
	}
	save("old", "a", now.Add(-3*24*time.Hour))
	save("recent", "a", now.Add(-time.Minute))
	save("foreign", "b", now.Add(-time.Minute))
	if err := s.ResourceAuditBatch(t.Context(), []domain.AuditEvent{{Action: "resource.rejected", TargetID: "a", CreatedAt: now.Add(-3 * 24 * time.Hour)}, {Action: "resource.observed", TargetID: "a", CreatedAt: now.Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	first, err := s.ReadUsageOutcomes(t.Context(), []string{"a"}, false, from.UnixMilli(), now.UnixMilli(), nil, 5*time.Minute)
	if err != nil || len(first.Failures) != 2 || len(first.Rejections) != 1 {
		t.Fatalf("full outcome scope: %+v %v", first, err)
	}
	save("late", "a", now.Add(-2*24*time.Hour))
	save("new", "a", now.Add(time.Minute))
	if err := s.ResourceAuditBatch(t.Context(), []domain.AuditEvent{{Action: "resource.rejected", TargetID: "a", CreatedAt: now.Add(-2 * 24 * time.Hour)}, {Action: "resource.rejected", TargetID: "b", CreatedAt: now.Add(-2 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	window := &OutcomeReadWindow{FromMS: from.UnixMilli(), ToMS: now.UnixMilli(), ReadAtMS: now.UnixMilli(), FailureCursor: first.FailureCursor, AuditCursor: first.AuditCursor}
	batch, err := s.ReadUsageOutcomes(t.Context(), []string{"a"}, false, from.Add(2*time.Minute).UnixMilli(), now.Add(2*time.Minute).UnixMilli(), window, 5*time.Minute)
	if err != nil || len(batch.Failures) != 3 || len(batch.Rejections) != 1 {
		t.Fatalf("delta=%+v err=%v", batch, err)
	}
	for _, r := range batch.Failures {
		if r.ID == "old" || r.UserID != "a" {
			t.Fatal("delta read untouched historical or foreign rows")
		}
	}
	if batch.Rejections[0].Calls != 1 || batch.Rejections[0].UserID != "a" {
		t.Fatal("late rejection duplicated or leaked scope")
	}
}
