package httpserver

import "cliproxy-portal/internal/webui"

// The two check buttons update only their own section. Results from the other
// section remain visible with their original checked-at time.
type systemCheckSnapshot struct {
	storage      []webui.HealthCheckView
	checks       []webui.HealthCheckView
	storageReady bool
	healthReady  bool
	retrying     bool
	lastSyncAt   string
	nextSyncAt   string
	syncInterval string
}

func (s *Server) mergeSystemChecks(next systemCheckSnapshot) systemCheckSnapshot {
	s.systemMu.Lock()
	defer s.systemMu.Unlock()
	if next.storageReady {
		s.systemChecks.storage = append([]webui.HealthCheckView(nil), next.storage...)
		s.systemChecks.storageReady = true
	}
	if next.healthReady {
		s.systemChecks.checks = append([]webui.HealthCheckView(nil), next.checks...)
		s.systemChecks.healthReady = true
		s.systemChecks.retrying = next.retrying
		s.systemChecks.lastSyncAt = next.lastSyncAt
		s.systemChecks.nextSyncAt = next.nextSyncAt
		s.systemChecks.syncInterval = next.syncInterval
	}
	result := s.systemChecks
	result.storage = append([]webui.HealthCheckView(nil), result.storage...)
	result.checks = append([]webui.HealthCheckView(nil), result.checks...)
	return result
}

func uncheckedSystemViews(names ...string) []webui.HealthCheckView {
	views := make([]webui.HealthCheckView, 0, len(names))
	for _, name := range names {
		views = append(views, webui.HealthCheckView{
			Component: name, Status: "warning", StatusLabel: "未检查",
			Metric: "—", Message: "尚未检查", CheckedAt: "—",
		})
	}
	return views
}

// Combine non-database storage using raw bytes, never rounded display strings.
// Keep the underlying check snapshot intact for independent refreshes.
func compactStorageViews(items []webui.HealthCheckView) []webui.HealthCheckView {
	var result []webui.HealthCheckView
	other := webui.HealthCheckView{Component: "其他存储", Status: "healthy", StatusLabel: "正常", Message: "交互记录、CPA 与容器日志合计", Metric: "—"}
	complete, count := true, 0
	var total int64
	for _, item := range items {
		if item.Component == "门户 SQLite" || item.Component == "CPAMP SQLite" {
			result = append(result, item)
			continue
		}
		count++
		if item.CheckedAt != "" && (other.CheckedAt == "" || item.CheckedAt < other.CheckedAt) {
			other.CheckedAt = item.CheckedAt
		}
		if item.Status == "error" {
			other.Status, other.StatusLabel = "error", "异常"
		} else if item.Status != "healthy" && other.Status != "error" {
			other.Status, other.StatusLabel = "warning", "需关注"
		}
		if !item.StorageSizeKnown || item.StorageBytes < 0 || total > (1<<63-1)-item.StorageBytes {
			complete = false
			continue
		}
		total += item.StorageBytes
	}
	if count == 0 {
		return result
	}
	if complete {
		other.StorageBytes, other.StorageSizeKnown, other.Metric = total, true, fileSizeLabel(total)
	} else {
		other.Message = "交互记录及日志占用暂未全部获取"
		if other.Status == "healthy" {
			other.Status, other.StatusLabel = "warning", "需关注"
		}
	}
	return append(result, other)
}
