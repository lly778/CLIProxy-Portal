package httpserver

import (
	"encoding/json"
	"strings"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

// Only protection metadata is persisted; never read a rejected request body to
// obtain a model name or token count.
type resourceRequestDetail struct {
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	StatusCode int    `json:"status_code,omitempty"`
}

func resourceRejectionStatus(reason string) int {
	switch reason {
	case "global_concurrency", "user_concurrency":
		return 429
	case "request_too_large":
		return 413
	case "cpu_pressure", "memory_pressure", "capture_backlog", "monitor_unavailable":
		return 503
	}
	// A WebSocket close is not an HTTP error. Old malformed-body events did
	// not retain whether they returned 400 or 413, so leave their status unknown.
	return 0
}

func resourceRequestEvent(e domain.AuditEvent) cpamp.EventRow {
	var detail resourceRequestDetail
	if json.Unmarshal([]byte(e.Detail), &detail) != nil || detail.Message == "" {
		detail.Message = strings.TrimSpace(e.Detail)
		// Older protection records have plain Chinese details. Preserve them
		// in the request log without changing storage or seven-day counters.
		switch detail.Message {
		case "全局并发已满", "用户并发已满":
			detail.StatusCode = 429
		case "整机内存达到总上限", "CPU 持续繁忙", "CPA套件内存达到上限", "CPA 套件内存达到上限", "CPA套件 CPU 持续繁忙", "CPA 套件 CPU 持续繁忙", "交互记录待处理过多", "资源监测未连接":
			detail.StatusCode = 503
		}
	}
	observed := e.Action == "resource.observed"
	row := cpamp.EventRow{PortalResourceID: e.ID, PortalUserID: e.TargetID,
		PortalUserLabel: e.TargetLabel, PortalResourceObserved: observed,
		TimestampMS: e.CreatedAt.UnixMilli(), Model: "—", Failed: !observed,
		FailSummary: detail.Message}
	if !observed && detail.StatusCode >= 400 && detail.StatusCode <= 599 {
		code := int64(detail.StatusCode)
		row.FailStatusCode = &code
	}
	return row
}
