package httpserver

import (
	"fmt"
	"math"
	"strings"

	"cliproxy-portal/internal/webui"
)

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
