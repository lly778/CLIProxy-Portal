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

func TestWeeklyTimelineHasFourPointsPerDayAndPreservesTotals(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, zone)
	a := cpamp.AnalyticsResponse{Granularity: "hour"}
	for i := 0; i < 168; i++ {
		a.Timeline = append(a.Timeline, cpamp.UsageTimelinePoint{BucketMS: from.Add(time.Duration(i) * time.Hour).UnixMilli(), Calls: 1, TotalTokens: 1000})
	}
	points := s.usageTimeline(a, from, from.Add(7*24*time.Hour))
	if len(points) != 28 || usageTrend(points).GranularityLabel != "每 6 小时" {
		t.Fatalf("weekly timeline has %d points with wrong granularity", len(points))
	}
	for i, point := range points {
		if point.Date != from.Add(time.Duration(i)*6*time.Hour).Format("01-02 15:00") || point.RequestValue != 6 || point.TokenValue != 6000 {
			t.Fatalf("weekly interval %d = %#v", i, point)
		}
	}
}

func TestWeeklyTimelineIncludesEmptyAndPartialBoundaryIntervals(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 25, 3, 15, 0, 0, zone)
	to := from.Add(7 * 24 * time.Hour)
	a := cpamp.AnalyticsResponse{Granularity: "hour", Timeline: []cpamp.UsageTimelinePoint{
		{BucketMS: from.Truncate(time.Hour).UnixMilli(), Calls: 2, TotalTokens: 200},
		{BucketMS: to.Truncate(time.Hour).UnixMilli(), Calls: 5, TotalTokens: 500},
		{BucketMS: from.Truncate(time.Hour).Add(-time.Hour).UnixMilli(), Calls: 99, TotalTokens: 9900},
		{BucketMS: to.Truncate(time.Hour).Add(time.Hour).UnixMilli(), Calls: 99, TotalTokens: 9900},
	}}
	points := s.usageTimeline(a, from, to)
	if len(points) != 29 || points[0].RequestValue != 2 || points[len(points)-1].RequestValue != 5 {
		t.Fatalf("partial boundary interval data lost: %#v", points)
	}
	for _, point := range points[1 : len(points)-1] {
		if point.RequestValue != 0 || point.TokenValue != 0 {
			t.Fatalf("empty interval contains fabricated data: %#v", point)
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
	for _, count := range []int{7, 24, 25, 28, 29, 30} {
		points := make([]webui.UsagePointView, count)
		for i := range points {
			points[i].Date = "10-02 12:00"
		}
		trend := usageTrend(points)
		previous, labels := -1, 0
		for i, point := range trend.Points {
			if !point.ShowLabel {
				continue
			}
			if previous >= 0 && i-previous < (count-1)/5 {
				t.Fatalf("%d points: adjacent labels at %d and %d", count, previous, i)
			}
			previous = i
			labels++
		}
		if labels != 6 || !trend.Points[0].ShowLabel || !trend.Points[count-1].ShowLabel {
			t.Fatalf("%d points: expected six labels including both boundaries", count)
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
