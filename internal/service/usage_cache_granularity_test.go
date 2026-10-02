package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

type granularityEchoCPAMP struct {
	cpamp.API
	calls int
}

func (f *granularityEchoCPAMP) Analytics(_ context.Context, req cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	f.calls++
	return cpamp.AnalyticsResponse{Granularity: req.Include.Granularity}, nil
}

func TestUsageCachesSeparateHourlyAndDailyWithinSameMinute(t *testing.T) {
	for _, scope := range []string{"global", "user"} {
		for _, order := range []bool{false, true} {
			t.Run(scope+map[bool]string{false: "/day-first", true: "/hour-first"}[order], func(t *testing.T) {
				accounts, st := accountsForTest(t)
				u, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
				if err != nil {
					t.Fatal(err)
				}
				from := time.Date(2026, 9, 26, 12, 0, 10, 0, time.UTC)
				if err := st.CreateKey(t.Context(), domain.APIKey{ID: "key", UserID: u.ID, Hash: strings.Repeat("a", 64), Status: "active", IssuedAt: from}); err != nil {
					t.Fatal(err)
				}
				fake := &granularityEchoCPAMP{}
				keys := NewKeys(st, fake)
				query := func(hourly bool) {
					to := from.Add(7 * 24 * time.Hour)
					want := "hour"
					if !hourly {
						to = to.Add(time.Nanosecond)
						want = "day"
					}
					var got cpamp.AnalyticsResponse
					var err error
					if scope == "global" {
						got, err = keys.GlobalUsage(t.Context(), from, to, 0)
					} else {
						got, err = keys.Usage(t.Context(), u.ID, from, to, 0)
					}
					if err != nil || got.Granularity != want {
						t.Fatalf("cache returned %q, want %q: %v", got.Granularity, want, err)
					}
				}
				query(order)
				query(!order)
				query(order)
				query(!order)
				if fake.calls != 2 {
					t.Fatalf("cache hits lost: %d calls, want 2", fake.calls)
				}
			})
		}
	}
}
