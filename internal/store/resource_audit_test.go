package store

import (
	"cliproxy-portal/internal/domain"
	"path/filepath"
	"testing"
	"time"
)

func TestResourceCountsUseRollingSevenDaysAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	start := now.Add(-7 * 24 * time.Hour)
	var events []domain.AuditEvent
	for _, e := range []struct {
		action string
		at     time.Time
	}{
		{"resource.rejected", start.Add(-time.Millisecond)},
		{"resource.rejected", start},
		{"resource.rejected", now.Add(-time.Hour)},
		{"resource.observed", now},
		{"resource.observed", now.Add(time.Millisecond)},
		{"resource.policy.update", now},
	} {
		events = append(events, domain.AuditEvent{ActorLabel: "资源保护", Action: e.action, CreatedAt: e.at})
	}
	if err := s.ResourceAuditBatch(t.Context(), events); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	counts, err := s.ResourceCounts(t.Context(), now)
	if err != nil || counts.Rejected != 2 || counts.Observed != 1 {
		t.Fatalf("wrong seven-day counts after reopen: %+v, %v", counts, err)
	}
	counts, err = s.ResourceCounts(t.Context(), now.Add(time.Millisecond))
	if err != nil || counts.Rejected != 1 || counts.Observed != 2 {
		t.Fatalf("window did not roll past the exact boundary: %+v, %v", counts, err)
	}
}
