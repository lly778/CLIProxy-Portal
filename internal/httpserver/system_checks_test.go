package httpserver

import (
	"reflect"
	"testing"

	"cliproxy-portal/internal/webui"
)

func TestCompactStorageUsesRawBytesAndPreservesChecks(t *testing.T) {
	items := []webui.HealthCheckView{
		{Component: "门户 SQLite", Metric: "30.5 MB"},
		{Component: "CPAMP SQLite", Metric: "612.8 MB"},
	}
	for _, name := range []string{"交互记录存储", "CPA 主日志", "CPA 请求/响应日志", "容器运行日志"} {
		items = append(items, webui.HealthCheckView{Component: name, Metric: "1.0 KB", Status: "healthy", StorageBytes: 1050, StorageSizeKnown: true, CheckedAt: "2026-10-06 00:25"})
	}
	original := append([]webui.HealthCheckView(nil), items...)
	compact := compactStorageViews(items)
	if len(compact) != 3 || !reflect.DeepEqual(items, original) || compact[0] != items[0] || compact[1] != items[1] {
		t.Fatal("compact display must not modify original checks or database cards")
	}
	other := compact[2]
	if other.Component != "其他存储" || other.Metric != "4.1 KB" || other.StorageBytes != 4200 || other.Status != "healthy" {
		t.Fatalf("must add exact bytes, not rounded 4.0 KB display values: %#v", other)
	}
	items[3].Status = "error"
	items[4].Status = "warning"
	if compactStorageViews(items)[2].Status != "error" {
		t.Fatal("aggregate must retain worst status")
	}
	items[3].StorageSizeKnown = false
	if got := compactStorageViews(items)[2]; got.Metric != "—" || got.StorageSizeKnown || got.Status != "error" {
		t.Fatalf("incomplete total must not be shown as complete: %#v", got)
	}
	unchecked := compactStorageViews(uncheckedSystemViews("门户 SQLite", "CPAMP SQLite", "交互记录存储", "CPA 主日志", "CPA 请求/响应日志", "容器运行日志"))
	if len(unchecked) != 3 || unchecked[2].Status != "warning" || unchecked[2].Metric != "—" {
		t.Fatalf("unchecked summary: %#v", unchecked)
	}
	if len(compactStorageViews(nil)) != 0 {
		t.Fatal("empty storage should stay empty")
	}
}

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
