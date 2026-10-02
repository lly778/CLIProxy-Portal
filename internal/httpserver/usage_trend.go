package httpserver

import (
	"fmt"
	"math"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/webui"
)

// usageTrendAxisScale rounds up to five readable intervals. Each series has its
// own scale, while integer steps avoid fractional request counts on the axis.
func usageTrendAxisScale(maximum int64) float64 {
	if maximum <= 5 {
		return 5
	}
	step := float64(maximum) / 5
	magnitude := math.Pow(10, math.Floor(math.Log10(step)))
	for _, unit := range []float64{1, 2, 2.5, 5, 10} {
		if unit*magnitude >= step {
			return math.Ceil(unit*magnitude) * 5
		}
	}
	return math.Ceil(step) * 5
}

// usageTimeline groups CPAMP's hourly data into local six-hour intervals for
// week-long ranges. Empty intervals are included; partially covered boundary
// intervals retain their data rather than dropping requests at either end.
func (s *Server) usageTimeline(a cpamp.AnalyticsResponse, from, to time.Time) []webui.UsagePointView {
	step := time.Duration(0)
	if to.After(from) && to.Sub(from) <= 7*24*time.Hour && !strings.EqualFold(a.Granularity, "day") {
		step = time.Hour
		if to.Sub(from) > 48*time.Hour {
			step = 6 * time.Hour
		}
	}
	for _, point := range a.Timeline {
		if point.BucketMS <= 0 {
			step = 0 // Preserve older responses with labels but no timestamps.
			break
		}
	}
	if len(a.Timeline) == 0 && a.Summary == nil {
		return nil
	}
	timeline := a.Timeline
	if step > 0 {
		local := from.In(s.Cfg.TimeZone)
		hours := int(step / time.Hour)
		start := time.Date(local.Year(), local.Month(), local.Day(), local.Hour()/hours*hours, 0, 0, 0, s.Cfg.TimeZone)
		grouped := make(map[int64]cpamp.UsageTimelinePoint)
		for _, point := range timeline {
			at := time.UnixMilli(point.BucketMS)
			if !at.Before(to) || !at.Add(time.Hour).After(from) {
				continue
			}
			index := int64(at.Sub(start) / step)
			bucket := start.Add(time.Duration(index) * step).UnixMilli()
			value := grouped[bucket]
			value.Calls += point.Calls
			value.TotalTokens += point.TotalTokens
			grouped[bucket] = value
		}
		timeline = nil
		for at := start; at.Before(to); at = at.Add(step) {
			point := grouped[at.UnixMilli()]
			point.BucketMS = at.UnixMilli()
			timeline = append(timeline, point)
		}
	}
	maxCalls, maxTokens := int64(1), int64(1)
	for _, point := range timeline {
		maxCalls = max(maxCalls, point.Calls)
		maxTokens = max(maxTokens, point.TotalTokens)
	}
	layout := "01-02"
	if step > 0 || strings.EqualFold(a.Granularity, "hour") {
		layout = "01-02 15:00"
	}
	points := make([]webui.UsagePointView, 0, len(timeline))
	for _, point := range timeline {
		label := point.Label
		if point.BucketMS > 0 {
			label = time.UnixMilli(point.BucketMS).In(s.Cfg.TimeZone).Format(layout)
		}
		points = append(points, webui.UsagePointView{BucketHours: int(step / time.Hour), Date: label, Requests: compactNumber(point.Calls), Tokens: compactNumber(point.TotalTokens), Percent: int(point.Calls * 100 / maxCalls), TokenPercent: int(point.TotalTokens * 100 / maxTokens), RequestValue: point.Calls, TokenValue: point.TotalTokens})
	}
	return points
}

// smoothUsageTrendPath interpolates the measured points with a monotone cubic
// curve. Flat intervals and local extrema have zero tangents, and each segment's
// control points stay within its endpoints' range so smoothing cannot overshoot.
func smoothUsageTrendPath(points []webui.UsageTrendPointView, value func(webui.UsageTrendPointView) int) string {
	if len(points) == 0 {
		return ""
	}
	var path strings.Builder
	fmt.Fprintf(&path, "M%d,%d", points[0].X, value(points[0]))
	if len(points) == 1 {
		return path.String()
	}
	if len(points) == 2 {
		fmt.Fprintf(&path, " L%d,%d", points[1].X, value(points[1]))
		return path.String()
	}

	widths := make([]float64, len(points)-1)
	slopes := make([]float64, len(points)-1)
	tangents := make([]float64, len(points))
	for i := range widths {
		widths[i] = float64(points[i+1].X - points[i].X)
		if widths[i] > 0 {
			slopes[i] = float64(value(points[i+1])-value(points[i])) / widths[i]
		}
	}
	tangents[0] = slopes[0]
	tangents[len(tangents)-1] = slopes[len(slopes)-1]
	for i := 1; i < len(points)-1; i++ {
		before, after := slopes[i-1], slopes[i]
		if widths[i-1] <= 0 || widths[i] <= 0 || before == 0 || after == 0 || math.Signbit(before) != math.Signbit(after) {
			continue
		}
		average := (before*widths[i] + after*widths[i-1]) / (widths[i-1] + widths[i])
		limit := math.Min(math.Min(math.Abs(before), math.Abs(after)), math.Abs(average)/2)
		tangents[i] = math.Copysign(2*limit, before)
	}
	for i, width := range widths {
		if width <= 0 {
			// Rounded coordinates can coincide in a very long custom date range.
			fmt.Fprintf(&path, " L%d,%d", points[i+1].X, value(points[i+1]))
			continue
		}
		x0, y0 := float64(points[i].X), float64(value(points[i]))
		x1, y1 := float64(points[i+1].X), float64(value(points[i+1]))
		third := width / 3
		fmt.Fprintf(&path, " C%.2f,%.2f %.2f,%.2f %d,%d", x0+third, y0+tangents[i]*third, x1-third, y1-tangents[i+1]*third, points[i+1].X, value(points[i+1]))
	}
	return path.String()
}
