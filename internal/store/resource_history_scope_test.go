package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/resourcecontrol"
)

func TestMixedHistoryDoesNotReuseEarlierMetricScopes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	legacy, err := json.Marshal([]resourcecontrol.HistoryPoint{{At: time.Now(), CPUPercent: 99, UsedMiB: 7000, TotalMiB: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"resource_history_v1", "resource_history_suite_v1"} {
		if _, err = s.db.Exec("INSERT INTO settings(key,value) VALUES(?,?)", key, string(legacy)); err != nil {
			t.Fatal(err)
		}
	}
	points, err := s.ResourceHistory(t.Context())
	if err != nil || len(points) != 0 {
		t.Fatal("old metric scope loaded as host CPU / suite memory history", err)
	}
	want := []resourcecontrol.HistoryPoint{{At: time.Now(), CPUPercent: 2, UsedMiB: 300, TotalMiB: 8192}}
	if err = s.SaveResourceHistory(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	points, err = s.ResourceHistory(t.Context())
	if err != nil || len(points) != 1 || points[0].UsedMiB != 300 {
		t.Fatal("suite history did not persist", err)
	}
	for _, key := range []string{"resource_history_v1", "resource_history_suite_v1"} {
		var original string
		if err = s.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&original); err != nil || original != string(legacy) {
			t.Fatal("legacy series was overwritten", key, err)
		}
	}
	points, err = s.ResourceHistoryForScope(t.Context(), "esxi")
	if err != nil || len(points) != 0 {
		t.Fatal("VM history reused for ESXi host", err)
	}
	host := []resourcecontrol.HistoryPoint{{At: time.Now(), CPUPercent: 12, UsedMiB: 14000, TotalMiB: 24420}}
	if err = s.SaveResourceHistoryForScope(t.Context(), host, "esxi"); err != nil {
		t.Fatal(err)
	}
	points, err = s.ResourceHistoryForScope(t.Context(), "esxi")
	if err != nil || len(points) != 1 || points[0].TotalMiB != 24420 {
		t.Fatal("ESXi host history did not persist", points, err)
	}
}
