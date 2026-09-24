package httpserver

import (
	"testing"

	"cliproxy-portal/internal/webui"
)

func TestSystemCheckSnapshotKeepsTheOtherSection(t *testing.T) {
	s := &Server{}
	first := s.mergeSystemChecks(systemCheckSnapshot{
		storage: []webui.HealthCheckView{{Component: "门户 SQLite", Metric: "1 MB"}}, storageReady: true,
		checks: []webui.HealthCheckView{{Component: "模型网关", Status: "healthy"}}, healthReady: true,
		lastSyncAt: "首次检查", retrying: true,
	})
	if !first.storageReady || !first.healthReady {
		t.Fatalf("initial snapshot = %#v", first)
	}
	storageOnly := s.mergeSystemChecks(systemCheckSnapshot{
		storage: []webui.HealthCheckView{{Component: "门户 SQLite", Metric: "2 MB"}}, storageReady: true,
	})
	if storageOnly.storage[0].Metric != "2 MB" || storageOnly.checks[0].Status != "healthy" || !storageOnly.retrying || storageOnly.lastSyncAt != "首次检查" {
		t.Fatalf("storage check changed health snapshot: %#v", storageOnly)
	}
	healthOnly := s.mergeSystemChecks(systemCheckSnapshot{
		checks: []webui.HealthCheckView{{Component: "模型网关", Status: "error"}}, healthReady: true,
		lastSyncAt: "再次检查", retrying: false,
	})
	if healthOnly.storage[0].Metric != "2 MB" || healthOnly.checks[0].Status != "error" || healthOnly.retrying || healthOnly.lastSyncAt != "再次检查" {
		t.Fatalf("health check changed storage snapshot: %#v", healthOnly)
	}
}
