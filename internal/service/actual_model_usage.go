package service

import (
	"context"
	"time"

	"cliproxy-portal/internal/cpamp"
)

const actualModelPageSize = 5000

// actualModelUsage maintains compact model statistics between refreshes. A
// first read, periodic reconciliation, manual refresh or failed delta check
// walks the entire selected range. Incomplete rankings are never published.
func (k *Keys) actualModelUsage(ctx context.Context, req cpamp.AnalyticsRequest, value cpamp.AnalyticsResponse, previewLimit int, forceFull ...bool) cpamp.AnalyticsResponse {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	page := value.Events
	value.ModelStats, value.ModelStatsNote = nil, ""
	value.ModelStatsReconcileAtMS = 0
	key := k.actualModelScopeKey(req)
	finish := func() cpamp.AnalyticsResponse { return analyticsPreview(value, previewLimit) }
	unavailable := func() cpamp.AnalyticsResponse {
		k.dropActualModelSnapshot(key)
		value.ModelStatsNote = "实际模型记录不完整，暂时无法按实际模型汇总。"
		return finish()
	}
	if page == nil || value.Summary == nil || rawModelHistoryDeleted(value) {
		return unavailable()
	}
	forced := len(forceFull) > 0 && forceFull[0]
	if !forced {
		if previous := k.currentActualModelSnapshot(req); previous != nil {
			if next, ok := k.incrementActualModels(ctx, req, value, previous); ok {
				value.ModelStats = next.models()
				value.ModelStatsReconcileAtMS = next.fullAt.Add(actualModelReconcile).UnixMilli()
				k.putActualModelSnapshot(key, next)
				return finish()
			}
		}
	}
	// A failed rebuild must not allow another refresh to reuse stale totals.
	k.dropActualModelSnapshot(key)
	next := newActualModelSnapshot(req, k.Now())
	var beforeMS, beforeID int64
	for pages := 0; ; pages++ {
		for _, event := range page.Items {
			next.addFull(event)
		}
		if !page.HasMore {
			break
		}
		if pages >= 199 || !nextModelPage(page, beforeMS, beforeID) {
			return unavailable()
		}
		beforeMS, beforeID = page.NextBeforeMS, page.NextBeforeID
		req.Include = cpamp.AnalyticsInclude{EventsPage: &cpamp.EventsPage{Limit: actualModelPageSize, BeforeMS: &beforeMS, BeforeID: &beforeID}}
		response, err := k.CPAMP.Analytics(ctx, req)
		if err != nil || response.Events == nil || rawModelHistoryDeleted(response) {
			return unavailable()
		}
		page = response.Events
	}
	if !next.matches(value.Summary) {
		return unavailable()
	}
	value.ModelStats = next.models()
	value.ModelStatsReconcileAtMS = next.fullAt.Add(actualModelReconcile).UnixMilli()
	k.putActualModelSnapshot(key, next)
	return finish()
}

func rawModelHistoryDeleted(value cpamp.AnalyticsResponse) bool {
	return value.Coverage != nil && value.Coverage.RawDeletedEventCount > 0
}

func nextModelPage(page *cpamp.EventsResponse, beforeMS, beforeID int64) bool {
	return len(page.Items) > 0 && page.NextBeforeMS > 0 && page.NextBeforeID > 0 &&
		(beforeMS == 0 || page.NextBeforeMS < beforeMS || page.NextBeforeMS == beforeMS && page.NextBeforeID < beforeID)
}
