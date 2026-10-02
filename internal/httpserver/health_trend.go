package httpserver

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

func (s *Server) requestHealthTrend(ctx context.Context, points []webui.UsagePointView, from, to time.Time, userID string) webui.HealthTrendView {
	if len(points) == 0 {
		return webui.HealthTrendView{}
	}
	var totals []store.GatewayTimingTotal
	var err error
	if points[0].BucketMS > 0 {
		totals, err = s.Store.GatewayTimingTotals(ctx, from, to, userID, time.UnixMilli(points[0].BucketMS), time.Duration(points[0].BucketHours)*time.Hour)
	}
	trend := healthTrend(points, totals)
	if err != nil {
		s.Logger.Error("query health trend timing", "error", err)
		trend.TimingError = "总耗时统计暂时不可用"
	}
	return trend
}

func durationLabel(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0f ms", ms)
	}
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", ms/1000), "0"), ".") + " s"
}

func (s *Server) averageTotalDuration(ctx context.Context, from, to time.Time, userID string) string {
	total, err := s.Store.GatewayTimingSummary(ctx, from, to, userID)
	if err != nil {
		s.Logger.Error("query average total duration", "error", err)
		return "—"
	}
	if total.Samples == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f ms", float64(total.TotalMS)/float64(total.Samples))
}

func healthTrend(points []webui.UsagePointView, totals []store.GatewayTimingTotal) webui.HealthTrendView {
	base := usageTrend(points)
	trend := webui.HealthTrendView{ShowSymbols: base.ShowSymbols}
	byBucket := make(map[int64]store.GatewayTimingTotal, len(totals))
	var maxAverage float64
	for _, total := range totals {
		byBucket[total.BucketMS] = total
		if total.Samples > 0 {
			maxAverage = math.Max(maxAverage, float64(total.TotalMS)/float64(total.Samples))
		}
	}
	const top, bottom = 28, 218
	scale := usageTrendAxisScale(max(1000, int64(math.Ceil(maxAverage))))
	for i := 0; i <= 5; i++ {
		trend.AxisTicks = append(trend.AxisTicks, webui.UsageTrendAxisTickView{Y: bottom - i*(bottom-top)/5, Requests: fmt.Sprintf("%d%%", i*20), Tokens: durationLabel(scale * float64(i) / 5)})
	}
	var segment []webui.UsageTrendPointView
	flush := func() {
		trend.SuccessPath += smoothUsageTrendPath(segment, func(p webui.UsageTrendPointView) int { return p.RequestY }) + " "
		trend.FailurePath += smoothUsageTrendPath(segment, func(p webui.UsageTrendPointView) int { return p.TokenY }) + " "
		segment = nil
	}
	for i, point := range points {
		view := webui.HealthTrendPointView{UsageTrendPointView: base.Points[i], SuccessRate: "—", FailureRate: "—", AverageTotal: "—", Samples: "0", SuccessY: bottom, FailureY: bottom, AverageUpload: "—", AverageWait: "—", AverageResponse: "—"}
		view.TokenY, view.BarHeight = bottom, 0
		view.HasRequests = point.RequestValue > 0
		if view.HasRequests {
			success := math.Max(0, math.Min(1, float64(point.SuccessValue)/float64(point.RequestValue)))
			failure := math.Max(0, math.Min(1, float64(point.FailureValue)/float64(point.RequestValue)))
			view.SuccessRate, view.FailureRate = fmt.Sprintf("%.1f%%", success*100), fmt.Sprintf("%.1f%%", failure*100)
			view.SuccessY, view.FailureY = bottom-int(math.Round(success*(bottom-top))), bottom-int(math.Round(failure*(bottom-top)))
			segment = append(segment, webui.UsageTrendPointView{X: view.X, RequestY: view.SuccessY, TokenY: view.FailureY})
		} else {
			flush() // Match CPAMP: do not connect across buckets without requests.
		}
		if total := byBucket[point.BucketMS]; total.Samples > 0 {
			average := float64(total.TotalMS) / float64(total.Samples)
			view.HasTiming = true
			view.AverageTotal, view.Samples = durationLabel(average), number(total.Samples)
			view.TokenY = bottom - int(math.Round(average/scale*(bottom-top)))
			view.BarHeight = bottom - view.TokenY
			// A mixed legacy/new bucket must not use a smaller denominator for
			// its segments than for the total. Keep the original total bar until
			// every timed sample has a valid three-stage breakdown.
			if total.StageSamples == total.Samples && total.UploadMS+total.WaitMS+total.ResponseMS == total.TotalMS {
				view.HasStages = true
				view.AverageUpload = durationLabel(float64(total.UploadMS) / float64(total.Samples))
				view.AverageWait = durationLabel(float64(total.WaitMS) / float64(total.Samples))
				view.AverageResponse = durationLabel(float64(total.ResponseMS) / float64(total.Samples))
				values := []int64{total.UploadMS, total.WaitMS, total.ResponseMS}
				classes := []string{"duration-upload", "duration-wait", "duration-response"}
				view.Stages = stackedTrendBars(view.UsageTrendPointView, classes, values)
			}
		}
		view.RequestY = min(view.SuccessY, view.FailureY)
		trend.Points = append(trend.Points, view)
	}
	flush()
	trend.SuccessPath, trend.FailurePath = strings.TrimSpace(trend.SuccessPath), strings.TrimSpace(trend.FailurePath)
	return trend
}
