package store

import (
	"cliproxy-portal/internal/resourcecontrol"
	"path/filepath"
	"testing"
	"time"
)

func TestResourceHistorySurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(resourcecontrol.HistoryInterval)
	c := resourcecontrol.NewController(resourcecontrol.DefaultPolicy())
	c.SetHistorySink(s.SaveResourceHistory)
	c.UpdateMetrics(resourcecontrol.Metrics{At: now, HostID: "test-host", CPUCount: 1, CPUPercent: 85, TotalMiB: 926, AvailableMiB: 200}, now)
	c.FlushHistory(t.Context())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	points, err := s.ResourceHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	restarted := resourcecontrol.NewController(resourcecontrol.DefaultPolicy())
	restarted.RestoreHistory(points, now)
	got := restarted.History(now)
	if len(got) != 1 || got[0].CPUPercent != 85 || got[0].UsedMiB != 726 {
		t.Fatalf("history was lost on restart: %+v", got)
	}
}
