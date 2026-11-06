package httpserver

import (
	"context"
	"sort"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
)

// Supplement sanitized upstream metadata with local failures. Do not change
// token/model usage metrics or fabricate a CPA request ID for an unforwarded call.
func (s *Server) supplementGatewayFailures(ctx context.Context, page cpamp.EventsResponse, from, to time.Time, userID string, limit int) cpamp.EventsResponse {
	if s.Store == nil || limit <= 0 {
		return page
	}
	rows, count, err := s.Store.GatewayUntracedFailures(ctx, from, to, userID, limit)
	if err != nil {
		s.Logger.Warn("list gateway failure metadata", "error", err)
		rows, count = nil, 0
	}
	items := append([]cpamp.EventRow(nil), page.Items...)
	upstreamIDs := map[[2]string]bool{}
	for _, event := range page.Items {
		if event.RequestID != "" {
			upstreamIDs[[2]string{strings.ToLower(event.RequestID), strings.ToLower(event.APIKeyHash)}] = true
		}
	}
	for _, row := range rows {
		// An upstream event may arrive after the last failure reconciliation.
		// Keep its now-visible row instead of adding the local placeholder.
		if row.CPARequestID != "" && upstreamIDs[[2]string{strings.ToLower(row.CPARequestID), strings.ToLower(row.APIKeyHash)}] {
			count--
			continue
		}
		reason := "Upstream request failed"
		switch row.FailureReason {
		case "upload_interrupted", "upload_interrupted_legacy":
			reason = "Request upload incomplete"
		case "preparation_failed":
			reason = "Request preparation failed"
		case "idle_timeout":
			reason = "Gateway idle timeout: no I/O progress for 60 seconds"
		case "connection_interrupted":
			reason = "Connection interrupted or response incomplete"
		}
		e := cpamp.EventRow{PortalTimingID: row.ID, RequestID: row.CPARequestID, TimestampMS: row.StartedAt.UnixMilli(), APIKeyHash: row.APIKeyHash, Model: "—", Failed: true, FailSummary: reason}
		if row.StatusCode >= 400 {
			code := int64(row.StatusCode)
			e.FailStatusCode = &code
		}
		items = append(items, e)
	}
	resources, resourceCount, resourceErr := s.Store.ResourceRequestEvents(ctx, from, to, userID, limit)
	if resourceErr != nil {
		s.Logger.Warn("list resource request metadata", "error", resourceErr)
	} else {
		for _, event := range resources {
			items = append(items, resourceRequestEvent(event))
		}
		count += resourceCount
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].TimestampMS > items[j].TimestampMS })
	if len(items) > limit {
		items = items[:limit]
	}
	page.Items, page.TotalCount = items, page.TotalCount+count
	return page
}

func (s *Server) usageWithGatewayFailures(ctx context.Context, a cpamp.AnalyticsResponse, from, to time.Time, userID string, limit int) cpamp.AnalyticsResponse {
	page := cpamp.EventsResponse{}
	if a.Events != nil {
		page = *a.Events
	}
	page = s.supplementGatewayFailures(ctx, page, from, to, userID, limit)
	a.Events = &page
	return a
}
