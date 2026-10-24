package service

import (
	"context"
	"sort"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/store"
)

type usageTimingSnapshot struct {
	window    store.TimingReadWindow
	dueMS     int64
	updatedAt time.Time
	minutes   map[int64]store.GatewayTimingTotal
}

func validTimingTotal(total store.GatewayTimingTotal) bool {
	stages := total.UploadMS + total.WaitMS + total.ResponseMS
	return total.Samples > 0 && total.TotalMS >= 0 && total.StageSamples >= 0 && total.StageSamples <= total.Samples &&
		total.UploadMS >= 0 && total.WaitMS >= 0 && total.ResponseMS >= 0 && stages <= total.TotalMS &&
		(total.StageSamples != total.Samples || stages == total.TotalMS)
}

func addTimingTotal(left, right store.GatewayTimingTotal) store.GatewayTimingTotal {
	left.Samples += right.Samples
	left.TotalMS += right.TotalMS
	left.StageSamples += right.StageSamples
	left.UploadMS += right.UploadMS
	left.WaitMS += right.WaitMS
	left.ResponseMS += right.ResponseMS
	return left
}

// Local measurements are fetched inside the same analytics flight and cached
// response as upstream counts/models. An ordinary cached page cannot advance
// one side's window or refresh one side independently of the other.
func (k *Keys) withUsageTimings(ctx context.Context, key, userID string, req cpamp.AnalyticsRequest, value cpamp.AnalyticsResponse, refresh bool) cpamp.AnalyticsResponse {
	if k.Store == nil {
		return value
	}
	k.timingMu.Lock()
	previous := k.usageTimings[key]
	k.timingMu.Unlock()
	now := k.Now()
	full := refresh || previous == nil
	if previous != nil {
		full = full || now.UnixMilli() >= previous.dueMS || now.Before(previous.updatedAt) ||
			req.FromMS < previous.window.FromMS || req.ToMS < previous.window.ToMS || req.FromMS >= previous.window.ToMS ||
			value.ModelStatsReconcileAtMS > 0 && value.ModelStatsReconcileAtMS != previous.dueMS
	}
	var window *store.TimingReadWindow
	if !full {
		window = &previous.window
	}
	batch, err := k.Store.ReadUsageTimings(ctx, req.FromMS, req.ToMS, userID, window, actualModelOverlap)
	if err == nil && !full && batch.Cursor < previous.window.Cursor {
		full = true // A restored/rewritten sample table invalidates the cursor.
		batch, err = k.Store.ReadUsageTimings(ctx, req.FromMS, req.ToMS, userID, nil, actualModelOverlap)
	}
	if err != nil {
		k.timingMu.Lock()
		delete(k.usageTimings, key)
		k.timingMu.Unlock()
		value.TimingStats = &domain.UsageTimings{Note: "总耗时统计暂时不可用"}
		return value
	}
	next := &usageTimingSnapshot{window: store.TimingReadWindow{FromMS: req.FromMS, ToMS: req.ToMS, Cursor: batch.Cursor},
		updatedAt: now, minutes: make(map[int64]store.GatewayTimingTotal)}
	if !full {
		next.dueMS = previous.dueMS
		first := req.FromMS / store.TimingSampleBucketMS * store.TimingSampleBucketMS
		for at, total := range previous.minutes {
			if at > first && at < req.ToMS && at < batch.HeadFromMS {
				next.minutes[at] = total
			}
		}
	} else {
		next.dueMS = now.Add(actualModelReconcile).UnixMilli()
		if value.ModelStatsReconcileAtMS > 0 {
			next.dueMS = value.ModelStatsReconcileAtMS
		}
	}
	for _, total := range batch.Totals {
		if !validTimingTotal(total) {
			k.timingMu.Lock()
			delete(k.usageTimings, key)
			k.timingMu.Unlock()
			value.TimingStats = &domain.UsageTimings{Note: "总耗时统计暂时不可用"}
			return value
		}
		next.minutes[total.BucketMS] = total
	}
	buckets := make(map[int64]store.GatewayTimingTotal)
	stats := &domain.UsageTimings{ReconcileAtMS: next.dueMS}
	location := k.UsageTimingLocation
	if location == nil {
		location = time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	granularity := value.Granularity
	if granularity == "" {
		granularity = req.Include.Granularity
	}
	bucketHours := domain.UsageBucketHours(granularity, time.UnixMilli(req.FromMS), time.UnixMilli(req.ToMS))
	for at, total := range next.minutes {
		stats.Samples += total.Samples
		stats.TotalMS += total.TotalMS
		bucket := domain.UsageBucketStart(time.UnixMilli(at), bucketHours, location).UnixMilli()
		combined := addTimingTotal(buckets[bucket], total)
		combined.BucketMS = bucket
		buckets[bucket] = combined
	}
	for _, total := range buckets {
		stats.Buckets = append(stats.Buckets, total)
	}
	sort.Slice(stats.Buckets, func(i, j int) bool { return stats.Buckets[i].BucketMS < stats.Buckets[j].BucketMS })
	value.TimingStats = stats
	k.putUsageTimingSnapshot(key, next)
	return value
}

func (k *Keys) putUsageTimingSnapshot(key string, next *usageTimingSnapshot) {
	k.timingMu.Lock()
	defer k.timingMu.Unlock()
	if k.usageTimings == nil {
		k.usageTimings = make(map[string]*usageTimingSnapshot)
	}
	delete(k.usageTimings, key)
	if len(next.minutes) > actualModelMaxRows {
		return // Large historical ranges still return complete full results.
	}
	for {
		count := len(next.minutes)
		oldest := ""
		for scope, snapshot := range k.usageTimings {
			count += len(snapshot.minutes)
			if oldest == "" || snapshot.updatedAt.Before(k.usageTimings[oldest].updatedAt) {
				oldest = scope
			}
		}
		if len(k.usageTimings) < actualModelMaxScopes && count <= actualModelMaxTotalRows {
			break
		}
		delete(k.usageTimings, oldest)
	}
	k.usageTimings[key] = next
}
