package httpserver

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"cliproxy-portal/internal/webui"
)

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
