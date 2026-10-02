package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
)

const actualModelPageSize = 5000

// actualModelUsage reads the entire selected range using keyset pagination,
// not the capped request-log preview. Other analytics stay authoritative and
// unchanged. Incomplete history must never produce a misleading model ranking.
func (k *Keys) actualModelUsage(ctx context.Context, req cpamp.AnalyticsRequest, value cpamp.AnalyticsResponse, previewLimit int) cpamp.AnalyticsResponse {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	page := value.Events
	value.ModelStats = nil
	deferPreview := func() {
		if previewLimit <= 0 || value.Events == nil {
			value.Events = nil
		} else {
			items := value.Events.Items
			if len(items) > previewLimit {
				items = items[:previewLimit]
			}
			// Usage pages display a preview only; pagination belongs to Requests.
			value.Events = &cpamp.EventsResponse{Items: append([]cpamp.EventRow(nil), items...), TotalCount: value.Events.TotalCount}
		}
	}
	unavailable := func() cpamp.AnalyticsResponse {
		value.ModelStatsNote = "实际模型记录不完整，暂时无法按实际模型汇总。"
		deferPreview()
		return value
	}
	if page == nil || value.Summary == nil || (value.Coverage != nil && value.Coverage.RawDeletedEventCount > 0) {
		return unavailable()
	}
	byModel := map[string]*cpamp.ModelUsageStat{}
	var calls, tokens int64
	var beforeMS, beforeID int64
	// Bound remote work; refuse partial rankings for extremely large ranges.
	for pages := 0; ; pages++ {
		for _, event := range page.Items {
			model := strings.TrimSpace(event.ResolvedModel)
			if model == "" {
				model = strings.TrimSpace(event.ResponseModel)
			}
			if model == "" {
				model = "未记录实际模型"
			}
			stat := byModel[model]
			if stat == nil {
				stat = &cpamp.ModelUsageStat{Model: model}
				byModel[model] = stat
			}
			stat.Calls++
			if event.Failed {
				stat.FailureCalls++
			} else {
				stat.SuccessCalls++
			}
			stat.InputTokens += event.InputTokens
			stat.OutputTokens += event.OutputTokens
			stat.TotalTokens += event.TotalTokens
			calls++
			tokens += event.TotalTokens
		}
		if !page.HasMore {
			break
		}
		if pages >= 199 || len(page.Items) == 0 || page.NextBeforeMS <= 0 || page.NextBeforeID <= 0 ||
			(beforeMS != 0 && (page.NextBeforeMS > beforeMS || (page.NextBeforeMS == beforeMS && page.NextBeforeID >= beforeID))) {
			return unavailable()
		}
		beforeMS, beforeID = page.NextBeforeMS, page.NextBeforeID
		req.Include = cpamp.AnalyticsInclude{EventsPage: &cpamp.EventsPage{Limit: actualModelPageSize, BeforeMS: &beforeMS, BeforeID: &beforeID}}
		next, err := k.CPAMP.Analytics(ctx, req)
		if err != nil || next.Events == nil || (next.Coverage != nil && next.Coverage.RawDeletedEventCount > 0) {
			return unavailable()
		}
		page = next.Events
	}
	if calls != value.Summary.TotalCalls || tokens != value.Summary.TotalTokens {
		return unavailable()
	}
	for _, stat := range byModel {
		stat.SuccessRate = float64(stat.SuccessCalls) / float64(stat.Calls)
		value.ModelStats = append(value.ModelStats, *stat)
	}
	sort.Slice(value.ModelStats, func(i, j int) bool { return value.ModelStats[i].Model < value.ModelStats[j].Model })
	deferPreview()
	return value
}
