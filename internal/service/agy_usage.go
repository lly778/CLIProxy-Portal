package service

import (
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"context"
	"sort"
	"strings"
	"time"
)

// Native usage is carried alongside CPA's raw source until its reconciliation
// has finished. Mixing it earlier would invalidate CPA's incremental cursors.
func (k *Keys) readAGYUsage(ctx context.Context, req cpamp.AnalyticsRequest) (*cpamp.AnalyticsResponse, error) {
	if k.Store == nil {
		return nil, nil
	}
	f := req.Filters
	if len(f.Providers) > 0 && !containsFold(f.Providers, "agy") {
		return nil, nil
	}
	// Unknown dimensions must never expand a partial query to all AGY data.
	if len(f.Accounts)+len(f.CredentialIDs)+len(f.AuthFiles)+len(f.AuthIndices)+len(f.SourceHashes)+len(f.ProjectIDs)+len(f.HeaderErrorKinds)+len(f.HeaderErrorCodes)+len(f.HeaderQuotaPlans)+len(f.HeaderTraceIDs)+len(f.RequestTypes) > 0 || req.SearchQuery != "" || req.SearchAPIKeyHash != "" || f.MinLatencyMS > 0 || f.CacheStatus != "" || f.FailedOnly || f.IncludeFailed != nil {
		return nil, nil
	}
	value := &cpamp.AnalyticsResponse{Granularity: req.Include.Granularity, GeneratedAtMS: req.NowMS}
	if req.Include.Summary || req.Include.Timeline || req.Include.APIKeyStats {
		rows, e := k.Store.AGYAggregates(ctx, req.FromMS, req.ToMS, f.APIKeyHashes, f.Models...)
		if e != nil {
			return nil, e
		}
		summary := &cpamp.UsageSummary{}
		keys := map[string]cpamp.APIKeyUsageStat{}
		models := map[string]cpamp.ModelUsageStat{}
		points := map[int64]cpamp.UsageTimelinePoint{}
		duration, unknown := int64(0), int64(0)
		loc := k.UsageTimingLocation
		if loc == nil {
			loc = time.FixedZone("Asia/Shanghai", 8*3600)
		}
		hours := 1
		if req.Include.Granularity == "day" {
			hours = 24
		}
		for _, r := range rows {
			if len(f.Models) > 0 && !containsFold(f.Models, r.Model) {
				continue
			}
			summary.TotalCalls += r.Calls
			summary.SuccessCalls += r.Success
			summary.FailureCalls += r.Failure
			summary.InputTokens += r.Input
			summary.OutputTokens += r.Output
			summary.CacheReadTokens += r.Cache
			summary.ReasoningTokens += r.Reasoning
			summary.TotalTokens += r.Total
			duration += r.Duration
			unknown += r.Unknown
			if req.Include.APIKeyStats {
				v := keys[r.Key]
				v.APIKeyHash = r.Key
				v.Calls += r.Calls
				v.SuccessCalls += r.Success
				v.FailureCalls += r.Failure
				v.InputTokens += r.Input
				v.OutputTokens += r.Output
				v.TotalTokens += r.Total
				v.LastSeenMS = max(v.LastSeenMS, r.Last)
				keys[r.Key] = v
			}
			if req.Include.Summary {
				v := models[r.Model]
				v.Model = r.Model
				v.Calls += r.Calls
				v.SuccessCalls += r.Success
				v.FailureCalls += r.Failure
				v.InputTokens += r.Input
				v.OutputTokens += r.Output
				v.TotalTokens += r.Total
				models[r.Model] = v
			}
			if req.Include.Timeline {
				at := domain.UsageBucketStart(time.UnixMilli(r.At), hours, loc).UnixMilli()
				v := points[at]
				v.BucketMS = at
				v.Calls += r.Calls
				v.Success += r.Success
				v.Failure += r.Failure
				v.InputTokens += r.Input
				v.OutputTokens += r.Output
				v.TotalTokens += r.Total
				v.Tokens += r.Total
				cached, reasoning := r.Cache, r.Reasoning
				if v.CachedTokens != nil {
					cached += *v.CachedTokens
				}
				if v.ReasoningTokens != nil {
					reasoning += *v.ReasoningTokens
				}
				v.CachedTokens = &cached
				v.ReasoningTokens = &reasoning
				points[at] = v
			}
		}
		if summary.TotalCalls > 0 {
			avg := float64(duration) / float64(summary.TotalCalls)
			summary.AverageLatencyMS = &avg
			summary.SuccessRate = float64(summary.SuccessCalls) / float64(summary.TotalCalls)
		}
		if req.Include.Summary {
			value.Summary = summary
		}
		for _, v := range keys {
			if v.Calls > 0 {
				v.SuccessRate = float64(v.SuccessCalls) / float64(v.Calls)
			}
			value.APIKeyStats = append(value.APIKeyStats, v)
		}
		for _, v := range models {
			if v.Calls > 0 {
				v.SuccessRate = float64(v.SuccessCalls) / float64(v.Calls)
			}
			value.ModelStats = append(value.ModelStats, v)
		}
		for _, v := range points {
			if v.Calls > 0 {
				v.SuccessRate = float64(v.Success) / float64(v.Calls)
				v.FailureRate = float64(v.Failure) / float64(v.Calls)
			}
			value.Timeline = append(value.Timeline, v)
		}
		if unknown > 0 {
			value.AGYUsageNote = "部分 AGY 请求未提供 Token，用量可能不完整。"
		}
	}
	if req.Include.EventsPage != nil && req.Include.EventsPage.BeforeMS == nil {
		rows, count, e := k.Store.AGYRequestPage(ctx, req.FromMS, req.ToMS, f.APIKeyHashes, min(req.Include.EventsPage.Limit, 100), f.Models...)
		if e != nil {
			return nil, e
		}
		page := &cpamp.EventsResponse{TotalCount: count}
		for _, r := range rows {
			if len(f.Models) > 0 && !containsFold(f.Models, r.Model) {
				continue
			}
			latency := r.Duration
			known := r.Usage.Known
			e := cpamp.EventRow{PortalTimingID: r.TimingID, PortalUserID: r.User, PortalUsageKnown: &known, RequestID: "agy:" + r.TimingID, EventHash: "agy:" + r.TimingID, TimestampMS: r.At, APIKeyHash: r.Key, Model: r.Model, RequestedModel: r.Requested, ResolvedModel: r.Model, ResponseModel: r.Model, Source: "agy", Endpoint: "/v1/responses", Path: "/v1/responses", Method: "POST", ReasoningEffort: r.Effort, InputTokens: r.Usage.Input, OutputTokens: r.Usage.Output, CacheReadTokens: r.Usage.Cache, ReasoningTokens: r.Usage.Reasoning, TotalTokens: r.Usage.Total, LatencyMS: &latency, Failed: r.Failed, FailSummary: r.Reason}
			if r.Status >= 400 {
				code := int64(r.Status)
				e.FailStatusCode = &code
			}
			page.Items = append(page.Items, e)
		}
		value.Events = page
	}
	return value, nil
}
func containsFold(v []string, s string) bool {
	for _, x := range v {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
func withAGYUsage(value cpamp.AnalyticsResponse) cpamp.AnalyticsResponse {
	n := value.AGYSource
	if n == nil {
		return value
	}
	value.AGYSource = nil
	value.AGYUsageNote = n.AGYUsageNote
	if n.Summary != nil && value.Summary != nil {
		a, b := *value.Summary, *n.Summary
		latency := float64(0)
		if a.AverageLatencyMS != nil {
			latency += *a.AverageLatencyMS * float64(a.TotalCalls)
		}
		if b.AverageLatencyMS != nil {
			latency += *b.AverageLatencyMS * float64(b.TotalCalls)
		}
		a.TotalCalls += b.TotalCalls
		a.SuccessCalls += b.SuccessCalls
		a.FailureCalls += b.FailureCalls
		a.InputTokens += b.InputTokens
		a.OutputTokens += b.OutputTokens
		a.CachedTokens += b.CachedTokens
		a.CacheReadTokens += b.CacheReadTokens
		a.ReasoningTokens += b.ReasoningTokens
		a.TotalTokens += b.TotalTokens
		if a.TotalCalls > 0 {
			v := latency / float64(a.TotalCalls)
			a.AverageLatencyMS = &v
			a.SuccessRate = float64(a.SuccessCalls) / float64(a.TotalCalls)
		}
		a.P95LatencyMS = nil
		value.Summary = &a
	}
	keys := map[string]cpamp.APIKeyUsageStat{}
	for _, v := range value.APIKeyStats {
		keys[v.APIKeyHash] = v
	}
	for _, v := range n.APIKeyStats {
		a := keys[v.APIKeyHash]
		a.APIKeyHash = v.APIKeyHash
		a.Calls += v.Calls
		a.SuccessCalls += v.SuccessCalls
		a.FailureCalls += v.FailureCalls
		a.InputTokens += v.InputTokens
		a.OutputTokens += v.OutputTokens
		a.TotalTokens += v.TotalTokens
		a.LastSeenMS = max(a.LastSeenMS, v.LastSeenMS)
		if a.Calls > 0 {
			a.SuccessRate = float64(a.SuccessCalls) / float64(a.Calls)
		}
		keys[v.APIKeyHash] = a
	}
	value.APIKeyStats = nil
	for _, v := range keys {
		value.APIKeyStats = append(value.APIKeyStats, v)
	}
	value.ModelStats = append(append([]cpamp.ModelUsageStat(nil), value.ModelStats...), n.ModelStats...)
	points := map[int64]cpamp.UsageTimelinePoint{}
	for _, v := range value.Timeline {
		points[v.BucketMS] = v
	}
	for _, v := range n.Timeline {
		a := points[v.BucketMS]
		a.BucketMS = v.BucketMS
		a.Calls += v.Calls
		a.Success += v.Success
		a.Failure += v.Failure
		a.InputTokens += v.InputTokens
		a.OutputTokens += v.OutputTokens
		a.TotalTokens += v.TotalTokens
		a.Tokens += v.Tokens
		if v.CachedTokens != nil {
			x := *v.CachedTokens
			if a.CachedTokens != nil {
				x += *a.CachedTokens
			}
			a.CachedTokens = &x
		}
		if v.ReasoningTokens != nil {
			x := *v.ReasoningTokens
			if a.ReasoningTokens != nil {
				x += *a.ReasoningTokens
			}
			a.ReasoningTokens = &x
		}
		if a.Calls > 0 {
			a.SuccessRate = float64(a.Success) / float64(a.Calls)
			a.FailureRate = float64(a.Failure) / float64(a.Calls)
		}
		points[v.BucketMS] = a
	}
	value.Timeline = nil
	for _, v := range points {
		value.Timeline = append(value.Timeline, v)
	}
	sort.Slice(value.Timeline, func(i, j int) bool { return value.Timeline[i].BucketMS < value.Timeline[j].BucketMS })
	if n.Events != nil {
		page := cpamp.EventsResponse{}
		if value.Events != nil {
			page = *value.Events
		}
		limit := max(len(page.Items), len(n.Events.Items))
		page.Items = append(append([]cpamp.EventRow(nil), page.Items...), n.Events.Items...)
		page.TotalCount += n.Events.TotalCount
		sort.SliceStable(page.Items, func(i, j int) bool { return page.Items[i].TimestampMS > page.Items[j].TimestampMS })
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
		}
		value.Events = &page
	}
	return value
}
