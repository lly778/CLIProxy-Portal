package httpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

type healthStatusCPAMP struct {
	cpamp.API
	value cpamp.AnalyticsResponse
}

func (f *healthStatusCPAMP) Analytics(context.Context, cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	return f.value, nil
}

func TestGlobalSummaryAndHealthTrendUseSameRequestStatus(t *testing.T) {
	from := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	fake := &healthStatusCPAMP{value: cpamp.AnalyticsResponse{
		Granularity: "hour", Summary: &cpamp.UsageSummary{TotalCalls: 3373, SuccessCalls: 3358, FailureCalls: 15},
		APIKeyStats: []cpamp.APIKeyUsageStat{{Calls: 3373, SuccessCalls: 2696, FailureCalls: 677}},
		Timeline:    []cpamp.UsageTimelinePoint{{BucketMS: from.UnixMilli(), Calls: 1000, Success: 1000}, {BucketMS: from.Add(4 * time.Hour).UnixMilli(), Calls: 2373, Success: 2358, Failure: 15}},
	}}
	s := &Server{Cfg: config.Config{TimeZone: time.UTC}, Keys: service.NewKeys(nil, fake)}
	a, err := s.Keys.GlobalUsage(t.Context(), from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	summary, points, _, _ := s.usageViews(a, from, to)
	if summary.Successes != "3.4K" || summary.Failures != "15" {
		t.Fatalf("summary = %+v", summary)
	}
	var calls, successes, failures int64
	for _, p := range points {
		calls += p.RequestValue
		successes += p.SuccessValue
		failures += p.FailureValue
	}
	if calls != a.Summary.TotalCalls || successes != a.Summary.SuccessCalls || failures != a.Summary.FailureCalls {
		t.Fatal("three-hour chart totals disagree with summary")
	}
	trend := healthTrend(points, nil)
	if trend.Points[1].FailureRate != "0.6%" || trend.Points[1].SuccessRate != "99.4%" {
		t.Fatalf("bucket rates = %+v", trend.Points[1])
	}
}

func TestHealthTrendWeightedRatesAndMissingBuckets(t *testing.T) {
	s := &Server{Cfg: config.Config{TimeZone: time.UTC}}
	from := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	a := cpamp.AnalyticsResponse{Granularity: "hour", Timeline: []cpamp.UsageTimelinePoint{
		{BucketMS: from.UnixMilli(), Calls: 10, Success: 9, Failure: 1},
		{BucketMS: from.Add(time.Hour).UnixMilli(), Calls: 90, Success: 81, Failure: 9},
		{BucketMS: from.Add(6 * time.Hour).UnixMilli(), Calls: 10, Success: 5, Failure: 5},
	}}
	points := s.usageTimeline(a, from, from.Add(4*24*time.Hour))
	if points[0].SuccessValue != 90 || points[0].FailureValue != 10 || points[0].RequestValue != 100 {
		t.Fatalf("grouped rates = %+v", points[0])
	}
	trend := healthTrend(points, []store.GatewayTimingTotal{{BucketMS: from.UnixMilli(), Samples: 4, TotalMS: 10000}})
	first := trend.Points[0]
	if first.SuccessRate != "90.0%" || first.FailureRate != "10.0%" || first.AverageTotal != "2.5 s" || first.Samples != "4" {
		t.Fatalf("health values = %+v", first)
	}
	if trend.Points[1].HasRequests || trend.Points[1].SuccessRate != "—" || trend.Points[1].HasTiming || trend.Points[2].AverageTotal != "—" {
		t.Fatal("missing requests or timing must stay missing")
	}
	if strings.Count(trend.SuccessPath, "M") != 2 || strings.Count(trend.FailurePath, "M") != 2 {
		t.Fatalf("curves connect across empty buckets: %s / %s", trend.SuccessPath, trend.FailurePath)
	}
	if trend.AxisTicks[0].Requests != "0%" || trend.AxisTicks[5].Requests != "100%" {
		t.Fatalf("percentage axis = %+v", trend.AxisTicks)
	}
}

func TestAverageTotalDurationUsesPersistentMeasurementsAndExactRange(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{Store: db}
	from := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	if got := s.averageTotalDuration(t.Context(), from, from.Add(time.Hour), "a"); got != "—" {
		t.Fatalf("missing duration = %s", got)
	}
	for i, duration := range []int64{1000, 1000, 10000} {
		if err := db.SaveGatewayRequestTiming(t.Context(), store.GatewayRequestTiming{ID: string(rune('a' + i)), UserID: "a", StartedAt: from.Add(time.Duration(i) * time.Minute), TotalMS: duration}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.averageTotalDuration(t.Context(), from, from.Add(time.Hour), "a"); got != "4000 ms" {
		t.Fatalf("weighted average = %s", got)
	}
	if got := s.averageTotalDuration(t.Context(), from, from.Add(time.Minute), "a"); got != "1000 ms" {
		t.Fatalf("boundary average = %s", got)
	}
	if got := s.averageTotalDuration(t.Context(), from, from.Add(time.Hour), "b"); got != "—" {
		t.Fatalf("cross-user average = %s", got)
	}
}

func TestHealthTrendSymbolThresholdAndDurationLabels(t *testing.T) {
	for _, count := range []int{36, 37} {
		points := make([]webui.UsagePointView, count)
		if got := healthTrend(points, nil).ShowSymbols; got != (count <= 36) {
			t.Fatalf("%d symbols = %v", count, got)
		}
	}
	for value, want := range map[float64]string{0: "0 ms", 50: "50 ms", 1000: "1 s", 2500: "2.5 s", 80000: "80 s"} {
		if got := durationLabel(value); got != want {
			t.Fatalf("duration %v = %s, want %s", value, got, want)
		}
	}
}
