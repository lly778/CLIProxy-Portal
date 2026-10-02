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

type tokenBreakdown struct {
	input, cache, output, reasoning int64
	available                       bool
}

// Use non-overlapping parts, preserving the upstream total. Normally reasoning
// is included in output; also accept explicitly additive totals from older
// upstreams. Missing or inconsistent breakdowns remain unavailable, not zero.
func timelineTokenBreakdown(point cpamp.UsageTimelinePoint) tokenBreakdown {
	if point.ReasoningTokens == nil || point.InputTokens < 0 || point.OutputTokens < 0 || *point.ReasoningTokens < 0 {
		return tokenBreakdown{}
	}
	input, output, reasoning := point.InputTokens, point.OutputTokens, *point.ReasoningTokens
	if point.CachedTokens == nil && point.CacheReadTokens == nil && point.CacheCreationTokens == nil {
		return tokenBreakdown{}
	}
	var cache int64
	// CPAMP's aggregate fields already separate legacy cached tokens from
	// fine-grained cache reads/creation. All are part of normalized input.
	for _, value := range []*int64{point.CachedTokens, point.CacheReadTokens, point.CacheCreationTokens} {
		if value != nil {
			if *value < 0 {
				return tokenBreakdown{}
			}
			cache += *value
		}
	}
	if cache > input {
		return tokenBreakdown{}
	}
	if point.TotalTokens == input+output && reasoning <= output {
		output -= reasoning
	} else if point.TotalTokens != input+output+reasoning {
		return tokenBreakdown{}
	}
	return tokenBreakdown{input: input - cache, cache: cache, output: output, reasoning: reasoning, available: true}
}

// Both token and timing stacks share widths, contiguous boundaries, and only
// round the uppermost nonzero segment. The total bar height never changes.
func stackedTrendBars(point webui.UsageTrendPointView, classes []string, values []int64) []webui.TrendBarSegmentView {
	var total int64
	last := -1
	for i, value := range values {
		total += value
		if value > 0 {
			last = i
		}
	}
	if total <= 0 {
		return nil
	}
	y := float64(point.TokenY + point.BarHeight)
	var bars []webui.TrendBarSegmentView
	for i, value := range values {
		if value <= 0 {
			continue
		}
		height := float64(point.BarHeight) * float64(value) / float64(total)
		y -= height
		bars = append(bars, webui.TrendBarSegmentView{Class: classes[i], BarX: point.BarX, BarWidth: point.BarWidth, Y: y, Height: height, Square: i != last})
	}
	return bars
}

// usageTimeline displays hourly buckets through 72 hours, three-hour buckets
// through seven days, then daily buckets. CPAMP supplies hourly/daily totals;
// combine the hourly totals without changing either metric or boundary data.
func (s *Server) usageTimeline(a cpamp.AnalyticsResponse, from, to time.Time) []webui.UsagePointView {
	if len(a.Timeline) == 0 {
		return nil
	}
	bucketHours := 24
	if !strings.EqualFold(a.Granularity, "day") && to.Sub(from) <= 7*24*time.Hour {
		bucketHours = 1
		if to.Sub(from) > 72*time.Hour {
			bucketHours = 3
		}
	}
	hourly := bucketHours < 24
	sourceHourly := strings.EqualFold(a.Granularity, "hour") || (a.Granularity == "" && hourly)
	fill := to.After(from)
	for _, point := range a.Timeline {
		if point.BucketMS <= 0 {
			fill = false // Preserve older responses with labels but no timestamps.
			break
		}
	}
	floor := func(at time.Time) time.Time {
		local := at.In(s.Cfg.TimeZone)
		hour := 0
		if hourly {
			hour = local.Hour() / bucketHours * bucketHours
		}
		return time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, s.Cfg.TimeZone)
	}
	next := func(at time.Time) time.Time {
		if hourly {
			return at.Add(time.Duration(bucketHours) * time.Hour)
		}
		return at.AddDate(0, 0, 1)
	}
	timeline := a.Timeline
	breakdowns := make(map[int64]tokenBreakdown)
	if fill {
		start := floor(from)
		grouped := make(map[int64]cpamp.UsageTimelinePoint)
		for _, point := range timeline {
			sourceStart := time.UnixMilli(point.BucketMS).In(s.Cfg.TimeZone)
			sourceEnd := sourceStart.AddDate(0, 0, 1)
			if sourceHourly {
				sourceEnd = sourceStart.Add(time.Hour)
			}
			// Check original source buckets before grouping. Otherwise an hour
			// outside the range could sneak into a partial three-hour boundary.
			if !sourceStart.Before(to) || !sourceEnd.After(from) {
				continue
			}
			at := floor(sourceStart)
			bucket := at.UnixMilli()
			parts := timelineTokenBreakdown(point)
			combined, exists := breakdowns[bucket]
			if !exists {
				combined.available = true
			}
			combined.available = combined.available && parts.available
			combined.input += parts.input
			combined.cache += parts.cache
			combined.output += parts.output
			combined.reasoning += parts.reasoning
			breakdowns[bucket] = combined
			value := grouped[bucket]
			value.Calls += point.Calls
			value.Success += point.Success
			value.Failure += point.Failure
			value.TotalTokens += point.TotalTokens
			grouped[bucket] = value
		}
		timeline = nil
		for at := start; at.Before(to); at = next(at) {
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
	if hourly {
		layout = "01-02 15:00"
	}
	points := make([]webui.UsagePointView, 0, len(timeline))
	for _, point := range timeline {
		label := point.Label
		var labelTick int64
		if point.BucketMS > 0 {
			local := time.UnixMilli(point.BucketMS).In(s.Cfg.TimeZone)
			label = local.Format(layout)
			// Civil-time ticks anchor labels to local hours/dates without parsing
			// yearless display strings in the browser.
			labelTick = time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, time.UTC).Unix() / 3600
			labelTick /= int64(bucketHours)
		}
		parts := timelineTokenBreakdown(point)
		if fill {
			var exists bool
			parts, exists = breakdowns[point.BucketMS]
			if !exists {
				parts.available = true
			} // Empty filled intervals have known zero usage.
		}
		points = append(points, webui.UsagePointView{BucketMS: point.BucketMS, BucketHours: bucketHours, LabelTick: labelTick, Date: label, Requests: compactNumber(point.Calls), Tokens: compactNumber(point.TotalTokens), Percent: int(point.Calls * 100 / maxCalls), TokenPercent: int(point.TotalTokens * 100 / maxTokens), RequestValue: point.Calls, TokenValue: point.TotalTokens, SuccessValue: point.Success, FailureValue: point.Failure, HasTokenBreakdown: parts.available, InputTokenValue: parts.input, CacheTokenValue: parts.cache, OutputTokenValue: parts.output, ReasoningTokenValue: parts.reasoning})
	}
	return points
}

// usageTrendLabelStep selects a whole-number interval for at most six labels.
// Rounding separately selected indexes would mix adjacent time intervals.
func usageTrendLabelStep(span int64) int64 {
	for _, step := range []int64{1, 2, 3, 4, 6, 8, 12, 24, 48, 72, 168} {
		if step*5 >= span {
			return step
		}
	}
	return ((span + 119) / 120) * 24
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
