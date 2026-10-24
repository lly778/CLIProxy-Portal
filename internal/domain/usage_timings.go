package domain

import (
	"strings"
	"time"
)

// GatewayTimingTotal is a compact aggregation of the same timing cohort.
// Stage sums only include samples with complete, valid stage boundaries.
type GatewayTimingTotal struct {
	BucketMS                     int64
	Samples, TotalMS             int64
	StageSamples                 int64
	UploadMS, WaitMS, ResponseMS int64
}

// UsageTimings travels with one cached usage response. Aggregates use the
// page's chart buckets and time zone; the mean uses total/sample
// counts, never an unweighted average of bucket averages.
type UsageTimings struct {
	Buckets          []GatewayTimingTotal
	Samples, TotalMS int64
	ReconcileAtMS    int64
	Note             string
}

func UsageBucketHours(granularity string, from, to time.Time) int {
	if !strings.EqualFold(granularity, "day") && to.Sub(from) <= 7*24*time.Hour {
		if to.Sub(from) > 72*time.Hour {
			return 3
		}
		return 1
	}
	return 24
}

func UsageBucketStart(at time.Time, hours int, location *time.Location) time.Time {
	local := at.In(location)
	hour := 0
	if hours < 24 {
		hour = local.Hour() / hours * hours
	}
	return time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, location)
}
