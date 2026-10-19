package resourcecontrol

import (
	"context"
	"math"
	"time"
)

const HistoryWindow = time.Hour
const HistoryInterval = 10 * time.Second
const HistoryCapacity = int(HistoryWindow/HistoryInterval) + 1

// Each point contains independent CPU and memory peaks in a ten-second bucket.
type HistoryPoint struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	UsedMiB    float64   `json:"used_mib"`
	TotalMiB   float64   `json:"total_mib"`
}

type historyBuffer struct {
	points [HistoryCapacity]HistoryPoint
	next   int
	count  int
}

func (h *historyBuffer) add(p HistoryPoint) {
	if h.count > 0 {
		last := &h.points[(h.next+HistoryCapacity-1)%HistoryCapacity]
		if p.At.Before(last.At) {
			return
		}
		if p.At.Equal(last.At) {
			last.CPUPercent = math.Max(last.CPUPercent, p.CPUPercent)
			last.UsedMiB = math.Max(last.UsedMiB, p.UsedMiB)
			last.TotalMiB = math.Max(last.TotalMiB, p.TotalMiB)
			return
		}
	}
	h.points[h.next] = p
	h.next = (h.next + 1) % HistoryCapacity
	if h.count < HistoryCapacity {
		h.count++
	}
}

func validHistoryPoint(p HistoryPoint, now time.Time) bool {
	return !p.At.IsZero() && !p.At.Before(now.Add(-HistoryWindow)) && !p.At.After(now) &&
		!math.IsNaN(p.CPUPercent) && !math.IsInf(p.CPUPercent, 0) && p.CPUPercent >= 0 && p.CPUPercent <= 100 &&
		!math.IsNaN(p.UsedMiB) && !math.IsInf(p.UsedMiB, 0) && p.UsedMiB >= 0 &&
		!math.IsNaN(p.TotalMiB) && !math.IsInf(p.TotalMiB, 0) && p.TotalMiB > 0 && p.UsedMiB <= p.TotalMiB
}

func (c *Controller) RestoreHistory(points []HistoryPoint, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range points {
		if validHistoryPoint(p, now) {
			p.At = p.At.UTC().Truncate(HistoryInterval)
			c.history.add(p)
		}
	}
}

func (c *Controller) History(now time.Time) []HistoryPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	points := make([]HistoryPoint, 0, c.history.count)
	start := (c.history.next + HistoryCapacity - c.history.count) % HistoryCapacity
	for i := 0; i < c.history.count; i++ {
		p := c.history.points[(start+i)%HistoryCapacity]
		if validHistoryPoint(p, now) {
			points = append(points, p)
		}
	}
	return points
}

func (c *Controller) SetHistorySink(sink func(context.Context, []HistoryPoint) error) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.historySink = sink
}

func (c *Controller) FlushHistory(ctx context.Context) {
	c.historyFlushMu.Lock()
	defer c.historyFlushMu.Unlock()
	c.logMu.Lock()
	sink := c.historySink
	c.logMu.Unlock()
	if sink == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = sink(writeCtx, c.History(time.Now()))
}

func (c *Controller) runHistory(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.FlushHistory(ctx)
		}
	}
}
