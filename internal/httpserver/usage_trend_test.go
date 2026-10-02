package httpserver

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/webui"
)

func TestWeeklyTimelineGroupsThreeHourlyPointsAndPreservesTotals(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, zone)
	a := cpamp.AnalyticsResponse{Granularity: "hour"}
	for i := 0; i < 168; i++ {
		a.Timeline = append(a.Timeline, cpamp.UsageTimelinePoint{BucketMS: from.Add(time.Duration(i) * time.Hour).UnixMilli(), Calls: 1, TotalTokens: 1000})
	}
	points := s.usageTimeline(a, from, from.Add(7*24*time.Hour))
	if len(points) != 56 || usageTrend(points).GranularityLabel != "每 3 小时" || usageTrend(points).ShowSymbols {
		t.Fatalf("weekly timeline has %d points with wrong granularity", len(points))
	}
	for i, point := range points {
		if point.Date != from.Add(time.Duration(i)*3*time.Hour).Format("01-02 15:00") || point.RequestValue != 3 || point.TokenValue != 3000 || point.BucketHours != 3 {
			t.Fatalf("weekly interval %d = %#v", i, point)
		}
	}
}

func TestCustomTwoDayTimelineKeeps48HourlyIntervalsWithoutChangingTotals(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, zone)
	a := cpamp.AnalyticsResponse{Granularity: "hour"}
	var calls, tokens int64
	for i := 0; i < 48; i++ {
		point := cpamp.UsageTimelinePoint{BucketMS: from.Add(time.Duration(i) * time.Hour).UnixMilli(), Calls: int64(i), TotalTokens: int64(i) * 1000}
		a.Timeline = append(a.Timeline, point)
		calls += point.Calls
		tokens += point.TotalTokens
	}
	points := s.usageTimeline(a, from, from.Add(48*time.Hour))
	if len(points) != 48 {
		t.Fatalf("two-day timeline has %d points, want 48", len(points))
	}
	var chartCalls, chartTokens int64
	for _, point := range points {
		if point.BucketHours != 1 {
			t.Fatalf("custom interval is %d hours, want 1", point.BucketHours)
		}
		chartCalls += point.RequestValue
		chartTokens += point.TokenValue
	}
	if chartCalls != calls || chartTokens != tokens {
		t.Fatalf("aggregation changed totals: %d/%d calls, %d/%d Tokens", chartCalls, calls, chartTokens, tokens)
	}
}

func TestUsageTimelineGranularityAt72HourAndSevenDayBoundaries(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, zone)
	for _, tc := range []struct {
		name               string
		span               time.Duration
		bucketHours, count int
	}{
		{"24 hours", 24 * time.Hour, 1, 24},
		{"exactly 72 hours", 72 * time.Hour, 1, 72},
		{"just over 72 hours", 72*time.Hour + time.Second, 3, 25},
		{"73 hours", 73 * time.Hour, 3, 25},
		{"four days", 96 * time.Hour, 3, 32},
		{"exactly seven days", 168 * time.Hour, 3, 56},
		{"just over seven days", 168*time.Hour + time.Second, 24, 8},
		{"eight days", 192 * time.Hour, 24, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			to := from.Add(tc.span)
			a := cpamp.AnalyticsResponse{Granularity: "hour", Summary: &cpamp.UsageSummary{}}
			for at := from; at.Before(to); at = at.Add(time.Hour) {
				calls := int64(len(a.Timeline)%5 + 1)
				a.Timeline = append(a.Timeline, cpamp.UsageTimelinePoint{BucketMS: at.UnixMilli(), Calls: calls, TotalTokens: calls * 12345})
				a.Summary.TotalCalls += calls
				a.Summary.TotalTokens += calls * 12345
			}
			points := s.usageTimeline(a, from, to)
			if len(points) != tc.count {
				t.Fatalf("got %d points, want %d", len(points), tc.count)
			}
			var calls, tokens int64
			for i, p := range points {
				if p.BucketHours != tc.bucketHours {
					t.Fatalf("got %d-hour buckets, want %d", p.BucketHours, tc.bucketHours)
				}
				if i > 0 && p.LabelTick-points[i-1].LabelTick != 1 {
					t.Fatal("label ticks must follow grouped intervals")
				}
				calls += p.RequestValue
				tokens += p.TokenValue
			}
			if calls != a.Summary.TotalCalls || tokens != a.Summary.TotalTokens {
				t.Fatal("grouping changed request or Token totals")
			}
		})
	}
}

func TestThreeHourTimelineLabelsRemainClockAlignedAndUniform(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 25, 14, 15, 0, 0, zone)
	to := from.Add(96 * time.Hour)
	a := cpamp.AnalyticsResponse{Granularity: "hour", Timeline: []cpamp.UsageTimelinePoint{{BucketMS: from.Truncate(time.Hour).UnixMilli(), Calls: 10, TotalTokens: 1000}}}
	points := s.usageTimeline(a, from, to)
	if len(points) != 33 || points[0].Date != "09-25 12:00" || points[32].Date != "09-29 12:00" {
		t.Fatalf("three-hour boundaries = %#v", points)
	}
	var previousTick, gap int64
	for _, p := range usageTrend(points).Points {
		if !p.ShowLabel {
			continue
		}
		if previousTick != 0 {
			if gap != 0 && p.LabelTick-previousTick != gap {
				t.Fatal("three-hour labels have mixed intervals")
			}
			gap = p.LabelTick - previousTick
		}
		previousTick = p.LabelTick
	}
	if gap == 0 {
		t.Fatal("expected multiple grouped labels")
	}
}

func TestWeeklyTimelineIncludesEmptyAndPartialBoundaryIntervals(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 25, 4, 15, 0, 0, zone)
	to := from.Add(7 * 24 * time.Hour)
	a := cpamp.AnalyticsResponse{Granularity: "hour", Timeline: []cpamp.UsageTimelinePoint{
		{BucketMS: from.Truncate(time.Hour).UnixMilli(), Calls: 2, TotalTokens: 200},
		{BucketMS: to.Truncate(time.Hour).UnixMilli(), Calls: 5, TotalTokens: 500},
		{BucketMS: from.Truncate(time.Hour).Add(-time.Hour).UnixMilli(), Calls: 99, TotalTokens: 9900},
		{BucketMS: to.Truncate(time.Hour).Add(time.Hour).UnixMilli(), Calls: 99, TotalTokens: 9900},
	}}
	points := s.usageTimeline(a, from, to)
	if len(points) != 57 || points[0].RequestValue != 2 || points[len(points)-1].RequestValue != 5 {
		t.Fatalf("partial boundary interval data lost: %#v", points)
	}
	for _, point := range points[1 : len(points)-1] {
		if point.RequestValue != 0 || point.TokenValue != 0 {
			t.Fatalf("empty interval contains fabricated data: %#v", point)
		}
	}
}

func TestLongTimelineFillsDailyBucketsAndPreservesBoundaryTotals(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 2, 3, 15, 0, 0, zone)
	to := from.AddDate(0, 0, 30)
	floor := func(at time.Time) int64 {
		return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, zone).UnixMilli()
	}
	a := cpamp.AnalyticsResponse{Granularity: "day", Timeline: []cpamp.UsageTimelinePoint{
		{BucketMS: floor(to), Calls: 7, TotalTokens: 700},
		{BucketMS: floor(from), Calls: 2, TotalTokens: 200},
		{BucketMS: floor(from), Calls: 3, TotalTokens: 300},
		{BucketMS: floor(from.AddDate(0, 0, -1)), Calls: 99, TotalTokens: 9900},
	}}
	points := s.usageTimeline(a, from, to)
	if len(points) != 31 || points[0].Date != "09-02" || points[30].Date != "10-02" || points[0].RequestValue != 5 || points[30].RequestValue != 7 {
		t.Fatalf("daily boundary data lost: %#v", points)
	}
	var calls, tokens int64
	for _, point := range points {
		if point.BucketHours != 24 {
			t.Fatal("daily interval must be one local day")
		}
		calls += point.RequestValue
		tokens += point.TokenValue
	}
	if calls != 12 || tokens != 1200 || usageTrend(points).GranularityLabel != "按天" {
		t.Fatalf("daily totals changed: %d calls, %d Tokens", calls, tokens)
	}
}

func TestEmptyTimelineRemainsEmptyAndLegacyLabelsArePreserved(t *testing.T) {
	s := &Server{Cfg: config.Config{TimeZone: time.UTC}}
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if got := s.usageTimeline(cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{}}, from, from.Add(24*time.Hour)); len(got) != 0 {
		t.Fatal("empty analytics must not fabricate chart data")
	}
	a := cpamp.AnalyticsResponse{Timeline: []cpamp.UsageTimelinePoint{{Label: "10-01", Calls: 5, TotalTokens: 123}}}
	points := s.usageTimeline(a, from, from.Add(24*time.Hour))
	if len(points) != 1 || points[0].Date != "10-01" || points[0].RequestValue != 5 || points[0].TokenValue != 123 {
		t.Fatalf("legacy labels changed: %#v", points)
	}
}

func TestUsageTrendSymbolThresholdPreservesAllPoints(t *testing.T) {
	for _, count := range []int{1, 12, 24, 36, 37, 168} {
		points := make([]webui.UsagePointView, count)
		trend := usageTrend(points)
		if len(trend.Points) != count || trend.ShowSymbols != (count <= 36) {
			t.Fatalf("%d points: incorrect symbol threshold or discarded data", count)
		}
	}
}

func TestRolling24HourTimelineIncludesPreviousDayAndKeepsHourlyData(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	to := time.Date(2026, 10, 2, 21, 0, 0, 0, zone)
	from := to.Add(-24 * time.Hour)
	a := cpamp.AnalyticsResponse{Granularity: "hour", Timeline: []cpamp.UsageTimelinePoint{{BucketMS: from.UnixMilli(), Calls: 10, TotalTokens: 1000}}}
	points := s.usageTimeline(a, from, to)
	if len(points) != 24 || points[0].Date != "10-01 21:00" || points[0].RequestValue != 10 || points[23].Date != "10-02 20:00" || points[23].RequestValue != 0 {
		t.Fatalf("rolling 24-hour data = %#v", points)
	}
	if points[0].BucketHours != 1 || usageTrend(points).GranularityLabel != "按小时" {
		t.Fatal("rolling 24-hour chart must retain hourly granularity")
	}
}

func TestUsageTrendTimestampLabelsAreEvenlySpaced(t *testing.T) {
	for _, count := range []int{1, 7, 17, 20, 24, 25, 28, 29, 30, 168} {
		points := make([]webui.UsagePointView, count)
		for i := range points {
			points[i].Date = "10-02 12:00"
		}
		trend := usageTrend(points)
		previous, labels, gap := -1, 0, 0
		for i, point := range trend.Points {
			if !point.ShowLabel {
				continue
			}
			if previous >= 0 {
				if gap != 0 && i-previous != gap {
					t.Fatalf("%d points: mixed label gaps %d and %d", count, gap, i-previous)
				}
				gap = i - previous
			}
			previous = i
			labels++
		}
		if labels < 1 || labels > 6 {
			t.Fatalf("%d points: expected at most six fixed-interval labels, got %d", count, labels)
		}
	}
}

func TestUsageTrendLabelsAlignToLocalWholeHoursAcrossMidnight(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	for _, tc := range []struct {
		hour, count int
		want        []string
	}{
		{12, 20, []string{"09-29 12:00", "09-29 16:00", "09-29 20:00", "09-30 00:00", "09-30 04:00"}},
		{14, 17, []string{"09-29 16:00", "09-29 20:00", "09-30 00:00", "09-30 04:00"}},
	} {
		from := time.Date(2026, 9, 29, tc.hour, 0, 0, 0, zone)
		a := cpamp.AnalyticsResponse{Granularity: "hour", Timeline: []cpamp.UsageTimelinePoint{{BucketMS: from.UnixMilli(), Calls: 10, TotalTokens: 1000}}}
		points := s.usageTimeline(a, from, from.Add(time.Duration(tc.count)*time.Hour))
		trend := usageTrend(points)
		var labels []string
		for _, point := range trend.Points {
			if point.ShowLabel {
				labels = append(labels, point.Date)
			}
		}
		if strings.Join(labels, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("labels = %v, want %v", labels, tc.want)
		}
		if points[0].RequestValue != 10 || len(trend.Points) != tc.count {
			t.Fatal("label selection changed underlying data")
		}
	}
}

func TestDailyLabelTicksKeepConstantSpacingAcrossMonthAndYear(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	for _, from := range []time.Time{time.Date(2026, 9, 20, 0, 0, 0, 0, zone), time.Date(2026, 12, 20, 0, 0, 0, 0, zone)} {
		a := cpamp.AnalyticsResponse{Granularity: "day", Timeline: []cpamp.UsageTimelinePoint{{BucketMS: from.UnixMilli(), Calls: 1}}}
		points := s.usageTimeline(a, from, from.AddDate(0, 0, 30))
		var previous int64
		for i, point := range usageTrend(points).Points {
			if i > 0 && point.LabelTick-points[i-1].LabelTick != 1 {
				t.Fatal("local day ticks are not consecutive")
			}
			if !point.ShowLabel {
				continue
			}
			if previous != 0 && point.LabelTick-previous != 6 {
				t.Fatal("daily label interval changed across calendar boundary")
			}
			previous = point.LabelTick
		}
	}
}

func TestUsageTrendBarsStayInsidePlotAndMatchTokenValues(t *testing.T) {
	for _, count := range []int{1, 2, 24, 29, 90} {
		points := make([]webui.UsagePointView, count)
		for i := range points {
			points[i] = webui.UsagePointView{RequestValue: 100, TokenValue: int64(i) * 1000}
		}
		trend := usageTrend(points)
		for i, point := range trend.Points {
			if point.BarX < 100 || point.BarX+point.BarWidth > 900 || point.BarWidth <= 0 || point.TokenY+point.BarHeight != 218 || point.TokenY < 28 {
				t.Fatalf("%d points: invalid bar %#v", count, point)
			}
			if i == 0 && point.BarHeight != 0 {
				t.Fatal("zero Tokens must not produce a nonzero bar")
			}
			if i > 0 && trend.Points[i-1].BarX+trend.Points[i-1].BarWidth >= point.BarX {
				t.Fatal("adjacent Token bars overlap")
			}
		}
		if trend.AxisTicks[0].Requests != "0" || trend.AxisTicks[0].Tokens != "0" || trend.AxisTicks[5].Y != 28 || !strings.HasSuffix(trend.RequestAreaPath, " Z") {
			t.Fatal("missing zero baseline, axis labels or closed area")
		}
	}
}

func TestUsageTrendAxisScaleIsReadableAndNeverClipsData(t *testing.T) {
	for _, maximum := range []int64{0, 1, 5, 10, 11, 104, 400, 123456, 21000000} {
		scale := usageTrendAxisScale(maximum)
		if scale < float64(maximum) || scale <= 0 || math.Mod(scale/5, 1) != 0 {
			t.Fatalf("invalid axis scale %f for %d", scale, maximum)
		}
	}
}

func TestSmoothUsageTrendPathSparseData(t *testing.T) {
	value := func(p webui.UsageTrendPointView) int { return p.RequestY }
	for _, tc := range []struct {
		name   string
		points []webui.UsageTrendPointView
		want   string
	}{
		{"empty", nil, ""},
		{"single point", []webui.UsageTrendPointView{{X: 500, RequestY: 218}}, "M500,218"},
		{"two points", []webui.UsageTrendPointView{{X: 48, RequestY: 218}, {X: 952, RequestY: 28}}, "M48,218 L952,28"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := smoothUsageTrendPath(tc.points, value); got != tc.want {
				t.Fatalf("path = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSmoothUsageTrendPathPassesThroughPointsWithoutOvershoot(t *testing.T) {
	for _, values := range [][]int{
		{218, 218, 218, 218, 218, 218},
		{218, 28, 218, 30, 218, 218},
		{218, 217, 216, 100, 29, 28},
		{28, 29, 30, 150, 217, 218},
		{218, 100, 100, 28, 218, 218},
	} {
		points := make([]webui.UsageTrendPointView, len(values))
		for i, x := range []int{48, 90, 160, 600, 610, 952} {
			points[i] = webui.UsageTrendPointView{X: x, RequestY: values[i], TokenY: 246 - values[i]}
		}
		for _, value := range []func(webui.UsageTrendPointView) int{
			func(p webui.UsageTrendPointView) int { return p.RequestY },
			func(p webui.UsageTrendPointView) int { return p.TokenY },
		} {
			path := smoothUsageTrendPath(points, value)
			segments := strings.Split(path, " C")
			if len(segments) != len(points) {
				t.Fatalf("incorrect segment count in %q", path)
			}
			for i, segment := range segments[1:] {
				fields := strings.Fields(strings.ReplaceAll(segment, ",", " "))
				if len(fields) != 6 {
					t.Fatalf("invalid cubic segment %q", segment)
				}
				var coords [6]float64
				for j, field := range fields {
					parsed, err := strconv.ParseFloat(field, 64)
					if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
						t.Fatalf("invalid coordinate %q", field)
					}
					coords[j] = parsed
				}
				x0, y0 := float64(points[i].X), float64(value(points[i]))
				x1, y1 := float64(points[i+1].X), float64(value(points[i+1]))
				if coords[4] != x1 || coords[5] != y1 {
					t.Fatalf("curve misses measured point: %q", segment)
				}
				if coords[0] < x0 || coords[2] < coords[0] || coords[2] > x1 {
					t.Fatalf("curve reverses time: %q", segment)
				}
				low, high := math.Min(y0, y1), math.Max(y0, y1)
				previous := y0
				for sample := 0; sample <= 100; sample++ {
					u := float64(sample) / 100
					v := 1 - u
					current := v*v*v*y0 + 3*v*v*u*coords[1] + 3*v*u*u*coords[3] + u*u*u*y1
					if current < low-0.001 || current > high+0.001 || (y1 >= y0 && current < previous-0.001) || (y1 <= y0 && current > previous+0.001) {
						t.Fatalf("curve overshoots or creates a false extremum: %q at %f", segment, u)
					}
					previous = current
				}
			}
		}
	}
}

func TestSmoothUsageTrendPathHandlesCoincidentCoordinates(t *testing.T) {
	points := []webui.UsageTrendPointView{{X: 48, RequestY: 218}, {X: 48, RequestY: 28}, {X: 49, RequestY: 100}}
	path := smoothUsageTrendPath(points, func(p webui.UsageTrendPointView) int { return p.RequestY })
	if !strings.HasPrefix(path, "M48,218 L48,28 C") || strings.Contains(path, "NaN") || strings.Contains(path, "Inf") {
		t.Fatalf("invalid path for coincident coordinates: %q", path)
	}
}
