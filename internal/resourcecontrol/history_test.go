package resourcecontrol

import (
	"math"
	"testing"
	"time"
)

func TestHistoryKeepsBriefPeaksAndExpiresWhileIdle(t *testing.T) {
	c := NewController(DefaultPolicy())
	end := time.Now().UTC().Truncate(HistoryInterval)
	for at := end.Add(-2 * time.Hour); !at.After(end); at = at.Add(2 * time.Second) {
		m := goodMetrics(at)
		m.CPUPercent = 12
		c.UpdateMetrics(m, at)
	}
	spike := goodMetrics(end.Add(2 * time.Second))
	spike.CPUPercent, spike.AvailableMiB = 97.4, 120
	c.UpdateMetrics(spike, spike.At)
	low := goodMetrics(end.Add(4 * time.Second))
	low.CPUPercent = 3
	c.UpdateMetrics(low, low.At)
	now := end.Add(5 * time.Second)
	points := c.History(now)
	if len(points) != 360 || points[0].At.Before(now.Add(-time.Hour)) {
		t.Fatalf("history did not retain a bounded rolling hour: %d points", len(points))
	}
	last := points[len(points)-1]
	if last.CPUPercent != 97.4 || last.UsedMiB != 806 {
		t.Fatalf("brief peak disappeared after a lower sample: %+v", last)
	}
	points[len(points)-1].CPUPercent = 0
	if c.History(now)[len(points)-1].CPUPercent != 97.4 {
		t.Fatal("history reader changed the retained samples")
	}
	if len(c.History(end.Add(time.Hour+time.Second))) != 0 {
		t.Fatal("old metrics remained visible while the sampler was idle")
	}
}

func TestRestoredHistoryRejectsInvalidDataAndDoesNotFillGaps(t *testing.T) {
	now := time.Now().UTC().Truncate(HistoryInterval)
	c := NewController(DefaultPolicy())
	c.RestoreHistory([]HistoryPoint{
		{At: now.Add(-2 * time.Hour), CPUPercent: 20, UsedMiB: 500, TotalMiB: 926},
		{At: now.Add(-time.Minute), CPUPercent: 30, UsedMiB: 550, TotalMiB: 926},
		{At: now.Add(-30 * time.Second), CPUPercent: math.NaN(), UsedMiB: 550, TotalMiB: 926},
		{At: now, CPUPercent: 40, UsedMiB: 600, TotalMiB: 926},
		{At: now.Add(time.Minute), CPUPercent: 50, UsedMiB: 600, TotalMiB: 926},
	}, now)
	points := c.History(now)
	if len(points) != 2 || points[1].At.Sub(points[0].At) != time.Minute {
		t.Fatalf("restored metrics were invalid or missing data was invented: %+v", points)
	}
	bad := goodMetrics(now.Add(2 * time.Second))
	bad.Error = "sampler unavailable"
	c.UpdateMetrics(bad, bad.At)
	if len(c.History(bad.At)) != 2 || c.History(bad.At)[1].CPUPercent != 40 {
		t.Fatal("unavailable monitoring replaced a real sample")
	}
}
