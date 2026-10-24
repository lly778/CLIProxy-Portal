package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cliproxy-portal/internal/cpamp"
)

type analyticsFlight struct {
	done    chan struct{}
	value   cpamp.AnalyticsResponse
	err     error
	refresh bool
}

// Model statistics do not depend on the number of preview rows a page shows.
// Reuse one complete aggregation, then give each page its own bounded preview.
func analyticsPreview(value cpamp.AnalyticsResponse, limit int) cpamp.AnalyticsResponse {
	if limit <= 0 || value.Events == nil {
		value.Events = nil
		return value
	}
	page := *value.Events
	limit = min(limit, 100, len(page.Items))
	page.Items = append([]cpamp.EventRow(nil), page.Items[:limit]...)
	value.Events = &page
	return value
}

// Rolling UI ranges reuse results for CacheTTL rather than expiring at every
// wall-clock minute. Historical ranges retain their exact boundaries; range
// length, granularity and local date prevent incompatible cache reuse.
func analyticsRangeKey(now, from, to time.Time) string {
	span := to.Sub(from)
	nearNow := !to.Before(now.Add(-time.Minute)) && !to.After(now.Add(time.Second))
	if nearNow && (span == 24*time.Hour || span == 7*24*time.Hour || span == 30*24*time.Hour) {
		date := to.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
		return fmt.Sprintf("rolling:%d:%s:%s", span, usageGranularity(from, to), date)
	}
	return fmt.Sprintf("fixed:%d:%d:%s", from.UnixMilli(), to.UnixMilli(), usageGranularity(from, to))
}

// loadAnalytics shares one bounded query among callers for the same scope.
// Cancelling a page load releases that waiter without cancelling other pages.
// Only successful results enter the cache. Explicit refreshes bypass cached
// values but can join an existing query fetching fresh data for that scope.
func (k *Keys) loadAnalytics(ctx context.Context, key string, refresh bool, fetch func(context.Context) (cpamp.AnalyticsResponse, error)) (cpamp.AnalyticsResponse, error) {
	if err := ctx.Err(); err != nil {
		return cpamp.AnalyticsResponse{}, err
	}
	k.cacheMu.Lock()
	if !refresh && k.CacheTTL > 0 {
		if entry, ok := k.cache[key]; ok && k.Now().Before(entry.expiresAt) {
			k.cacheMu.Unlock()
			return entry.value, nil
		}
		delete(k.cache, key)
	}
	if k.analyticsFlights == nil {
		k.analyticsFlights = make(map[string]*analyticsFlight)
	}
	flight := k.analyticsFlights[key]
	if flight != nil && refresh && !flight.refresh {
		// A manual refresh must be able to force full model reconciliation,
		// even when an automatic incremental query is already in progress.
		k.cacheMu.Unlock()
		select {
		case <-ctx.Done():
			return cpamp.AnalyticsResponse{}, ctx.Err()
		case <-flight.done:
			return k.loadAnalytics(ctx, key, true, fetch)
		}
	}
	if flight == nil {
		flight = &analyticsFlight{done: make(chan struct{}), refresh: refresh}
		k.analyticsFlights[key] = flight
		go func() {
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
			defer cancel()
			var value cpamp.AnalyticsResponse
			var err error
			defer func() {
				if recover() != nil {
					value = cpamp.AnalyticsResponse{}
					err = errors.New("analytics query failed")
				}
				if err == nil && (value.TimingStats == nil || value.TimingStats.Note == "") {
					k.putCache(key, value)
				}
				k.cacheMu.Lock()
				flight.value, flight.err = value, err
				delete(k.analyticsFlights, key)
				close(flight.done)
				k.cacheMu.Unlock()
			}()
			value, err = fetch(fetchCtx)
			if err == nil {
				err = fetchCtx.Err()
			}
		}()
	}
	k.cacheMu.Unlock()
	select {
	case <-ctx.Done():
		return cpamp.AnalyticsResponse{}, ctx.Err()
	case <-flight.done:
		return flight.value, flight.err
	}
}
