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
