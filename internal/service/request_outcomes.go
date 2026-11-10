package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

// Called inside the usage flight. There is no separate result cache/TTL for
// outcomes: counts, tokens and statuses keep the same page/source cutoff.
func (k *Keys) requestLogSource(ctx context.Context, users []string, global bool, req cpamp.AnalyticsRequest, refresh bool) (*domain.RequestLogData, error) {
	if k.Store == nil {
		return nil, nil
	}
	return k.incrementUsageOutcomes(ctx, users, global, req, refresh)
}

func (k *Keys) reconcileRequestFailures(ctx context.Context, data *domain.RequestLogData, refresh bool) error {
	type group struct {
		from, to int64
		hashes   map[string]bool
		indices  []int
	}
	groups := map[int64]*group{}
	now := k.Now().UnixMilli()
	for i, r := range data.Failures {
		if r.RequestID == "" {
			continue
		}
		age := int64(time.Hour / time.Millisecond)
		if len(r.Upstream) == 0 {
			age = int64(time.Minute / time.Millisecond)
		}
		if !refresh && r.CheckedMS > 0 && now >= r.CheckedMS && now-r.CheckedMS < age {
			continue
		}
		bucket := r.StartedMS / int64(10*time.Minute/time.Millisecond)
		g := groups[bucket]
		if g == nil {
			g = &group{from: r.StartedMS - 1000, to: max(r.EndedMS, r.StartedMS) + 5000, hashes: map[string]bool{}}
			groups[bucket] = g
		}
		g.from = min(g.from, r.StartedMS-1000)
		g.to = max(g.to, max(r.EndedMS, r.StartedMS)+5000)
		g.hashes[r.APIKeyHash] = true
		g.indices = append(g.indices, i)
	}
	var updates []domain.GatewayFailure
	for _, g := range groups {
		hashes := []string{}
		for h := range g.hashes {
			hashes = append(hashes, h)
		}
		sort.Strings(hashes)
		req := cpamp.AnalyticsRequest{FromMS: max(1, g.from), ToMS: g.to, NowMS: now, TimeZone: "Asia/Shanghai", Filters: cpamp.AnalyticsFilters{APIKeyHashes: hashes}, Include: cpamp.AnalyticsInclude{EventsPage: &cpamp.EventsPage{Limit: 5000}}}
		found := map[[2]string][]domain.RequestOutcomeEvent{}
		var beforeMS, beforeID int64
		generated := int64(0)
		for pages := 0; ; pages++ {
			value, err := k.queryUsageAnalytics(ctx, req, false)
			if err != nil {
				return err
			}
			if value.Events == nil || rawModelHistoryDeleted(value) {
				return errors.New("请求结果明细暂时无法核对")
			}
			generated = max(generated, value.GeneratedAtMS)
			for _, e := range value.Events.Items {
				if !g.hashes[e.APIKeyHash] || e.TimestampMS < req.FromMS || e.TimestampMS >= req.ToMS {
					return errors.New("请求结果超出核对范围")
				}
				if e.RequestID != "" {
					identity := [2]string{strings.ToLower(e.RequestID), strings.ToLower(e.APIKeyHash)}
					found[identity] = append(found[identity], domain.RequestOutcomeEvent{Hash: e.EventHash, AtMS: e.TimestampMS, Failed: e.Failed})
				}
			}
			if !value.Events.HasMore {
				break
			}
			if pages >= 199 || !nextModelPage(value.Events, beforeMS, beforeID) {
				return errors.New("请求结果核对不完整")
			}
			beforeMS, beforeID = value.Events.NextBeforeMS, value.Events.NextBeforeID
			req.Include.EventsPage = &cpamp.EventsPage{Limit: 5000, BeforeMS: &beforeMS, BeforeID: &beforeID}
		}
		for _, i := range g.indices {
			r := &data.Failures[i]
			r.Upstream = found[[2]string{r.RequestID, r.APIKeyHash}]
			r.CheckedMS = now
			r.GeneratedMS = generated
			updates = append(updates, *r)
		}
	}
	if len(updates) > 0 {
		if err := k.Store.SaveFailureReconciliation(ctx, updates); err != nil {
			return err
		}
	}
	for _, r := range data.Failures {
		if r.RequestID != "" {
			if r.CheckedMS == 0 {
				data.Note = "部分门户失败尚未与上游记录匹配，正在核对请求结果"
			}
			data.RequiredUpstreamMS = max(data.RequiredUpstreamMS, r.GeneratedMS)
		}
	}
	return nil
}

func (k *Keys) queryUsageForRequestLog(ctx context.Context, req cpamp.AnalyticsRequest, userID string, refresh bool) (cpamp.AnalyticsResponse, error) {
	value, err := k.queryUsageAnalytics(ctx, req, refresh)
	if err != nil {
		return value, err
	}
	req.FromMS, req.ToMS = value.WindowFromMS, value.WindowToMS
	users := []string{}
	if userID != "" {
		users = append(users, userID)
	}
	data, err := k.requestLogSource(ctx, users, userID == "", req, refresh)
	if err != nil {
		// Preserve available token/model data, but never present partial request
		// outcomes as complete or cache the failed statistics query.
		value.RequestLogData = &domain.RequestLogData{Note: "请求结果统计暂时不可用"}
		return value, nil
	}
	if data != nil && data.RequiredUpstreamMS > 0 && value.GeneratedAtMS > 0 && value.GeneratedAtMS < data.RequiredUpstreamMS {
		value, err = k.queryUsageAnalytics(ctx, req, true)
		if err != nil {
			return value, err
		}
	}
	value.RequestLogData = data
	return value, nil
}

type requestDelta struct {
	at                     int64
	user, key              string
	calls, success, failed int64
}

func requestLogDeltas(data *domain.RequestLogData, from, to int64) []requestDelta {
	if data == nil {
		return nil
	}
	var result []requestDelta
	if data.Corrections != nil {
		for _, d := range data.Corrections {
			result = append(result, requestDelta{at: d.AtMS, user: d.UserID, key: d.APIKeyHash, calls: d.Calls, success: d.Success, failed: d.Failure})
		}
		return result
	}
	seen := map[string]bool{}
	for _, r := range data.Failures {
		if r.RequestID == "" || len(r.Upstream) == 0 && r.CheckedMS > 0 {
			identity := "local:" + r.ID
			if r.RequestID != "" {
				identity = "trace:" + r.RequestID + ":" + r.APIKeyHash
			}
			if seen[identity] {
				continue
			}
			seen[identity] = true
			if r.StartedMS >= from && r.StartedMS < to {
				result = append(result, requestDelta{at: r.StartedMS, user: r.UserID, key: r.APIKeyHash, calls: 1, failed: 1})
			}
			continue
		}
		for _, e := range r.Upstream {
			if e.AtMS < from || e.AtMS >= to || e.Failed {
				continue
			}
			identity := r.RequestID + ":" + r.APIKeyHash + ":" + e.Hash
			if e.Hash == "" {
				identity += fmt.Sprint(e.AtMS)
			}
			if seen[identity] {
				continue
			}
			seen[identity] = true
			result = append(result, requestDelta{at: e.AtMS, user: r.UserID, key: r.APIKeyHash, success: -1, failed: 1})
		}
	}
	for _, r := range data.Rejections {
		result = append(result, requestDelta{at: r.AtMS, user: r.UserID, calls: r.Calls, failed: r.Calls})
	}
	return result
}

// Apply after actual-model validation: model/token accounting continues to use
// raw upstream counts. Request status totals and their timeline share one union.
func (k *Keys) withRequestLogStatus(value cpamp.AnalyticsResponse, req cpamp.AnalyticsRequest) cpamp.AnalyticsResponse {
	data := value.RequestLogData
	if data == nil {
		return withAGYUsage(value)
	}
	value.RequestLogNote = data.Note
	deltas := requestLogDeltas(data, req.FromMS, req.ToMS)
	if value.Summary != nil {
		summary := *value.Summary
		for _, d := range deltas {
			summary.TotalCalls += d.calls
			summary.SuccessCalls += d.success
			summary.FailureCalls += d.failed
		}
		if summary.TotalCalls < 0 || summary.SuccessCalls < 0 || summary.FailureCalls < 0 || summary.SuccessCalls+summary.FailureCalls != summary.TotalCalls {
			value.RequestLogNote = "请求结果正在核对，请稍后刷新"
		} else {
			summary.SuccessRate = 0
			if summary.TotalCalls > 0 {
				summary.SuccessRate = float64(summary.SuccessCalls) / float64(summary.TotalCalls)
			}
			value.Summary = &summary
		}
	}
	users := map[string]int64{}
	for _, d := range deltas {
		users[d.user] += d.calls
	}
	for id, calls := range users {
		if calls != 0 {
			value.RequestLogUsers = append(value.RequestLogUsers, domain.RequestUserDelta{UserID: id, Calls: calls})
		}
	}
	if req.Include.Timeline {
		location := k.UsageTimingLocation
		if location == nil {
			location = time.FixedZone("Asia/Shanghai", 8*60*60)
		}
		hours := 1
		if req.Include.Granularity == "day" {
			hours = 24
		}
		points := map[int64]cpamp.UsageTimelinePoint{}
		for _, p := range value.Timeline {
			points[p.BucketMS] = p
		}
		for _, d := range deltas {
			at := domain.UsageBucketStart(time.UnixMilli(d.at), hours, location).UnixMilli()
			p := points[at]
			p.BucketMS = at
			p.Calls += d.calls
			p.Success += d.success
			p.Failure += d.failed
			if p.Calls > 0 {
				p.SuccessRate = float64(p.Success) / float64(p.Calls)
				p.FailureRate = float64(p.Failure) / float64(p.Calls)
			}
			points[at] = p
		}
		value.Timeline = nil
		for _, p := range points {
			if p.Calls < 0 || p.Success < 0 || p.Failure < 0 || p.Success+p.Failure != p.Calls {
				value.RequestLogNote = "请求结果正在核对，请稍后刷新"
			}
			value.Timeline = append(value.Timeline, p)
		}
		sort.Slice(value.Timeline, func(i, j int) bool { return value.Timeline[i].BucketMS < value.Timeline[j].BucketMS })
	}
	return withAGYUsage(value)
}

func (k *Keys) annotateRequestFailures(ctx context.Context, value cpamp.AnalyticsResponse) (cpamp.AnalyticsResponse, error) {
	value = withAGYUsage(value)
	if k.Store == nil || value.Events == nil {
		return value, nil
	}
	identities := make([][2]string, 0, len(value.Events.Items))
	for _, e := range value.Events.Items {
		if e.RequestID != "" {
			identities = append(identities, [2]string{strings.ToLower(e.RequestID), strings.ToLower(e.APIKeyHash)})
		}
	}
	reasons, err := k.Store.FailedRequestReasons(ctx, identities)
	if err != nil {
		return value, err
	}
	page := *value.Events
	page.Items = append([]cpamp.EventRow(nil), page.Items...)
	for i := range page.Items {
		e := &page.Items[i]
		if reason, ok := reasons[[2]string{strings.ToLower(e.RequestID), strings.ToLower(e.APIKeyHash)}]; ok {
			if e.Failed && reason != "idle_timeout" && reason != "connection_interrupted" {
				continue
			}
			e.Failed = true
			e.FailSummary = gatewayFailureMessage(reason)
			e.FailStatusCode = nil
		}
	}
	value.Events = &page
	return value, nil
}

func gatewayFailureMessage(reason string) string {
	switch reason {
	case "idle_timeout":
		return "Gateway idle timeout: no I/O progress for 60 seconds"
	case "upload_interrupted", "upload_interrupted_legacy":
		return "Request upload incomplete"
	case "preparation_failed":
		return "Request preparation failed"
	case "connection_interrupted":
		return "Connection interrupted or response incomplete"
	default:
		return "Upstream request failed"
	}
}
