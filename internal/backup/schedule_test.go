package backup

import (
	"testing"
	"time"
)

func TestWeeklyScheduleAndCatchUp(t *testing.T) {
	zone := time.FixedZone("Beijing", 8*60*60)
	c := Config{Enabled: true, Repository: "owner/private", Hour: 3, Weekday: 1}
	monday := time.Date(2026, 10, 5, 3, 0, 0, 0, zone)
	for _, test := range []struct {
		name string
		now  time.Time
		last string
		want bool
	}{
		{"before-hour", monday.Add(-time.Minute), "", false},
		{"scheduled", monday, "", true},
		{"catch-up-tuesday", monday.Add(24 * time.Hour), "2026-09-28", true},
		{"already-attempted", monday.Add(24 * time.Hour), "2026-10-05", false},
		{"attempted-late", monday.Add(72 * time.Hour), "2026-10-06", false},
		{"week-end", monday.Add(6 * 24 * time.Hour), "2026-10-05", false},
		{"next-week-before", monday.Add(7*24*time.Hour - time.Minute), "2026-10-05", false},
		{"next-week", monday.Add(7 * 24 * time.Hour), "2026-10-05", true},
		{"utc-boundary", monday.UTC(), "2026-09-28", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Due(c, Status{Repository: c.Repository, LastAttemptDay: test.last}, test.now); got != test.want {
				t.Fatalf("due=%v want=%v", got, test.want)
			}
		})
	}
	if next := NextScheduledAt(c, monday.Add(24*time.Hour)); !next.Equal(monday.Add(7 * 24 * time.Hour)) {
		t.Fatal("next run is not the next Monday", next)
	}
	c.Weekday = 0
	sunday := monday.AddDate(0, 0, 6)
	if Due(c, Status{}, monday) || !Due(c, Status{}, sunday) {
		t.Fatal("Sunday schedule must use end of the same calendar week")
	}
	c.Weekday = 7
	if Due(c, Status{}, monday) {
		t.Fatal("invalid weekday scheduled")
	}
}

func TestWeeklyScheduleMinutePrecision(t *testing.T) {
	zone := time.FixedZone("Beijing", 8*60*60)
	c := Config{Enabled: true, Repository: "owner/private", Weekday: 1, Hour: 3, Minute: 45, Retain: 3, KeySaved: true, Instance: "0123456789abcdef"}
	scheduled := time.Date(2026, 10, 5, 3, 45, 0, 0, zone)
	if Due(c, Status{}, scheduled.Add(-time.Second)) || !Due(c, Status{}, scheduled.UTC()) {
		t.Fatal("minute precision or Beijing timezone not honored")
	}
	if !ScheduledAt(c, scheduled.UTC()).Equal(scheduled) || !NextScheduledAt(c, scheduled).Equal(scheduled.AddDate(0, 0, 7)) {
		t.Fatal("scheduled minute lost")
	}
	for _, minute := range []int{-1, 60} {
		c.Minute = minute
		if Due(c, Status{}, scheduled) || Validate(c) == nil {
			t.Fatal("invalid minute accepted")
		}
	}
}
