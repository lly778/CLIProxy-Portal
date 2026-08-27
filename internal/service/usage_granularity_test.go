package service

import (
	"testing"
	"time"
)

func TestUsageGranularityUsesHoursForShortRanges(t *testing.T) {
	from := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	if got := usageGranularity(from, from.Add(48*time.Hour)); got != "hour" {
		t.Fatalf("48-hour granularity = %q", got)
	}
	if got := usageGranularity(from, from.Add(48*time.Hour+time.Second)); got != "day" {
		t.Fatalf("long-range granularity = %q", got)
	}
}
