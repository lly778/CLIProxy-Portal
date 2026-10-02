package httpserver

import (
	"math"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/webui"
)

func tokenPtr(v int64) *int64 { return &v }

func TestTimelineTokenBreakdownPreservesNonOverlappingTotals(t *testing.T) {
	base := cpamp.UsageTimelinePoint{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, ReasoningTokens: tokenPtr(15), CachedTokens: tokenPtr(10), CacheReadTokens: tokenPtr(50), CacheCreationTokens: tokenPtr(5)}
	for _, tc := range []struct {
		name   string
		change func(*cpamp.UsageTimelinePoint)
		want   tokenBreakdown
	}{
		{"inclusive", func(*cpamp.UsageTimelinePoint) {}, tokenBreakdown{35, 65, 25, 15, true}},
		{"additive reasoning", func(p *cpamp.UsageTimelinePoint) { p.TotalTokens = 155 }, tokenBreakdown{35, 65, 40, 15, true}},
		{"missing reasoning", func(p *cpamp.UsageTimelinePoint) { p.ReasoningTokens = nil }, tokenBreakdown{}},
		{"missing cache", func(p *cpamp.UsageTimelinePoint) {
			p.CachedTokens, p.CacheReadTokens, p.CacheCreationTokens = nil, nil, nil
		}, tokenBreakdown{}},
		{"negative cache", func(p *cpamp.UsageTimelinePoint) { p.CachedTokens = tokenPtr(-1) }, tokenBreakdown{}},
		{"excess cache", func(p *cpamp.UsageTimelinePoint) { p.CacheReadTokens = tokenPtr(100) }, tokenBreakdown{}},
		{"excess reasoning", func(p *cpamp.UsageTimelinePoint) { p.ReasoningTokens = tokenPtr(41) }, tokenBreakdown{}},
		{"wrong total", func(p *cpamp.UsageTimelinePoint) { p.TotalTokens = 139 }, tokenBreakdown{}},
		{"negative input", func(p *cpamp.UsageTimelinePoint) { p.InputTokens = -1 }, tokenBreakdown{}},
		{"negative output", func(p *cpamp.UsageTimelinePoint) { p.OutputTokens = -1 }, tokenBreakdown{}},
		{"negative reasoning", func(p *cpamp.UsageTimelinePoint) { p.ReasoningTokens = tokenPtr(-1) }, tokenBreakdown{}},
		{"known zero", func(p *cpamp.UsageTimelinePoint) {
			*p = cpamp.UsageTimelinePoint{ReasoningTokens: tokenPtr(0), CachedTokens: tokenPtr(0)}
		}, tokenBreakdown{available: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.change(&p)
			if got := timelineTokenBreakdown(p); got != tc.want {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestGroupedTokenPartsPreserveBoundsTotalsAndSourceValues(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	s := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 9, 26, 1, 0, 0, 0, zone)
	for _, span := range []time.Duration{7 * 24 * time.Hour, 8 * 24 * time.Hour} {
		point := func(at time.Time, additive bool) cpamp.UsageTimelinePoint {
			p := cpamp.UsageTimelinePoint{BucketMS: at.UnixMilli(), Calls: 1, TotalTokens: 140, InputTokens: 100, OutputTokens: 40, ReasoningTokens: tokenPtr(15), CacheReadTokens: tokenPtr(60)}
			if additive {
				p.TotalTokens = 155
			}
			return p
		}
		a := cpamp.AnalyticsResponse{Granularity: "hour", Timeline: []cpamp.UsageTimelinePoint{
			point(from.Add(-time.Hour), false), // Excluded even when it shares the display bucket.
			point(from, false), point(from.Add(time.Hour), true), point(from.Add(span), false),
		}}
		points := s.usageTimeline(a, from, from.Add(span))
		first := points[0]
		if !first.HasTokenBreakdown || first.RequestValue != 2 || first.TokenValue != 295 || first.InputTokenValue != 80 || first.CacheTokenValue != 120 || first.OutputTokenValue != 65 || first.ReasoningTokenValue != 30 {
			t.Fatalf("span %s: %#v", span, first)
		}
		for _, p := range points[1:] {
			if !p.HasTokenBreakdown || p.TokenValue != 0 {
				t.Fatal("empty filled intervals must remain known zero")
			}
		}
		if *a.Timeline[1].ReasoningTokens != 15 || a.Timeline[1].InputTokens != 100 {
			t.Fatal("source was mutated")
		}
		a.Timeline[2].ReasoningTokens = nil
		fallback := s.usageTimeline(a, from, from.Add(span))[0]
		if fallback.HasTokenBreakdown || fallback.TokenValue != 295 {
			t.Fatalf("mixed bucket must retain total-only fallback: %#v", fallback)
		}
	}
}

func TestTokenStackGeometrySharesTotalHeightWidthAndTopCorners(t *testing.T) {
	for _, parts := range [][4]int64{{35, 65, 25, 15}, {0, 100, 40, 0}, {140, 0, 0, 0}, {0, 0, 0, 0}} {
		p := webui.UsagePointView{RequestValue: 2, TokenValue: parts[0] + parts[1] + parts[2] + parts[3], HasTokenBreakdown: true, InputTokenValue: parts[0], CacheTokenValue: parts[1], OutputTokenValue: parts[2], ReasoningTokenValue: parts[3]}
		got := usageTrend([]webui.UsagePointView{p}).Points[0]
		p.HasTokenBreakdown = false
		legacy := usageTrend([]webui.UsagePointView{p}).Points[0]
		if !got.HasTokenBreakdown || got.BarHeight != legacy.BarHeight || got.BarWidth != legacy.BarWidth || got.TokenY != legacy.TokenY {
			t.Fatal("stack changed total geometry")
		}
		bottom := float64(got.TokenY + got.BarHeight)
		for i, segment := range got.TokenSegments {
			if math.Abs(segment.Y+segment.Height-bottom) > 1e-9 || segment.BarWidth != got.BarWidth || segment.BarX != got.BarX || segment.Square != (i < len(got.TokenSegments)-1) {
				t.Fatalf("bad segment: %#v", segment)
			}
			bottom = segment.Y
		}
		if math.Abs(bottom-float64(got.TokenY)) > 1e-9 {
			t.Fatal("stack height differs from total")
		}
		if got.OutputTokens != compactNumber(parts[2]+parts[3]) || len(got.TokenSegments) > 3 {
			t.Fatal("three-part stack must include reasoning in output")
		}
		if legacy.InputTokens != "—" || legacy.CacheTokens != "—" || legacy.OutputTokens != "—" {
			t.Fatal("unavailable parts must not show zero")
		}
	}
}
