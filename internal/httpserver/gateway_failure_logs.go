package httpserver

import (
	"context"
	"sort"
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
	for _, row := range rows {
		reason := "Upstream request failed"
		switch row.FailureReason {
		case "upload_interrupted", "upload_interrupted_legacy":
			reason = "Request upload incomplete"
		case "preparation_failed":
			reason = "Request preparation failed"
		}
		e := cpamp.EventRow{PortalTimingID: row.ID, TimestampMS: row.StartedAt.UnixMilli(), APIKeyHash: row.APIKeyHash, Model: "—", Failed: true, FailSummary: reason}
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
