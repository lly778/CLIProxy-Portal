package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/security"
)

// All monitoring reads use one source. Scope includes filters and time range;
// a more complete response can serve a lighter page without another RPC.
type usageSourceCache struct {
	mu     sync.Mutex
	scopes map[string]*usageSourceScope
}
type usageSourceScope struct {
	generation uint64
	updated    time.Time
	entries    []usageSourceEntry
	flights    []*usageSourceFlight
}
type usageSourceEntry struct {
	include cpamp.AnalyticsInclude
	value   cpamp.AnalyticsResponse
	expires time.Time
}
type usageSourceFlight struct {
	include    cpamp.AnalyticsInclude
	generation uint64
	done       chan struct{}
	value      cpamp.AnalyticsResponse
	err        error
}

func usageSourceKey(req cpamp.AnalyticsRequest) string {
	req.Filters.APIKeyHashes = append([]string(nil), req.Filters.APIKeyHashes...)
	sort.Strings(req.Filters.APIKeyHashes)
	req.FromMS, req.ToMS, req.NowMS = 0, 0, 0
	req.Include.Summary, req.Include.Timeline, req.Include.APIKeyStats = false, false, false
	req.Include.EventsPage = nil
	payload, _ := json.Marshal(req)
	return security.SHA256(string(payload))
}

func usageSourceCovers(have, want cpamp.AnalyticsInclude) bool {
	if want.Summary && !have.Summary || want.Timeline && !have.Timeline || want.APIKeyStats && !have.APIKeyStats {
		return false
	}
	if want.EventsPage != nil {
		if have.EventsPage == nil {
			return false
		}
		h, _ := json.Marshal(have.EventsPage)
		w, _ := json.Marshal(want.EventsPage)
		if string(h) != string(w) {
			return false
		}
	}
	return true
}

func usageSourceProjection(value cpamp.AnalyticsResponse, include cpamp.AnalyticsInclude) cpamp.AnalyticsResponse {
	if value.AGYSource != nil {
		native := usageSourceProjection(*value.AGYSource, include)
		value.AGYSource = &native
	}
	if !include.Summary {
		value.Summary = nil
	} else if value.Summary != nil {
		copy := *value.Summary
		value.Summary = &copy
	}
	if !include.Timeline {
		value.Timeline = nil
	} else {
		value.Timeline = append([]cpamp.UsageTimelinePoint(nil), value.Timeline...)
	}
	if !include.APIKeyStats {
		value.APIKeyStats = nil
	} else {
		value.APIKeyStats = append([]cpamp.APIKeyUsageStat(nil), value.APIKeyStats...)
	}
	if include.EventsPage == nil {
		value.Events = nil
	} else if value.Events != nil {
		page := *value.Events
		page.Items = append([]cpamp.EventRow(nil), page.Items...)
		value.Events = &page
	}
	return value
}

func (k *Keys) queryUsageAnalytics(ctx context.Context, req cpamp.AnalyticsRequest, refresh bool) (cpamp.AnalyticsResponse, error) {
	if err := ctx.Err(); err != nil {
		return cpamp.AnalyticsResponse{}, err
	}
	// History cursors and incremental pages must always read the selected records.
	if req.Include.EventsPage != nil && (req.Include.EventsPage.BeforeMS != nil || req.Include.EventsPage.BeforeID != nil) {
		return k.fetchUsageSource(ctx, req)
	}
	rangeKey := analyticsRangeKey(k.Now(), time.UnixMilli(req.FromMS), time.UnixMilli(req.ToMS))
	key := rangeKey + ":" + usageSourceKey(req)
	cache := &k.usageSource
	cache.mu.Lock()
	if cache.scopes == nil {
		cache.scopes = make(map[string]*usageSourceScope)
	}
	// A warm global key aggregate may serve a subset. A miss must still query
	// only the requested keys; never expand a partial request to all-user data.
	if !refresh && len(req.Filters.APIKeyHashes) > 0 && req.Include.APIKeyStats && !req.Include.Summary && !req.Include.Timeline && req.Include.EventsPage == nil {
		global := req
		global.Filters.APIKeyHashes = nil
		if shared := cache.scopes[rangeKey+":"+usageSourceKey(global)]; shared != nil {
			var value cpamp.AnalyticsResponse
			found := false
			for _, entry := range shared.entries {
				if k.CacheTTL > 0 && k.Now().Before(entry.expires) && usageSourceCovers(entry.include, req.Include) {
					value, found = entry.value, true
					break
				}
			}
			if found {
				cache.mu.Unlock()
				return usageSourceKeySubset(value, req), nil
			}
		}
	}
	scope := cache.scopes[key]
	if scope == nil {
		if len(cache.scopes) >= 64 {
			oldest := ""
			for id, candidate := range cache.scopes {
				if len(candidate.flights) == 0 && (oldest == "" || candidate.updated.Before(cache.scopes[oldest].updated)) {
					oldest = id
				}
			}
			if oldest != "" {
				delete(cache.scopes, oldest)
			}
		}
		scope = &usageSourceScope{}
		if len(cache.scopes) < 64 {
			cache.scopes[key] = scope
		}
	}
	scope.updated = k.Now()
	if refresh {
		scope.generation++
		scope.entries = nil
	} else if k.CacheTTL > 0 {
		for _, entry := range scope.entries {
			if k.Now().Before(entry.expires) && usageSourceCovers(entry.include, req.Include) {
				cache.mu.Unlock()
				return usageSourceProjection(entry.value, req.Include), nil
			}
		}
	}
	var flight *usageSourceFlight
	for _, candidate := range scope.flights {
		if candidate.generation == scope.generation && usageSourceCovers(candidate.include, req.Include) {
			flight = candidate
			break
		}
	}
	if flight == nil {
		flight = &usageSourceFlight{include: req.Include, generation: scope.generation, done: make(chan struct{})}
		scope.flights = append(scope.flights, flight)
		go func() {
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
			defer cancel()
			value, err := k.fetchUsageSource(fetchCtx, req)
			if err == nil {
				err = fetchCtx.Err()
			}
			value.WindowFromMS, value.WindowToMS = req.FromMS, req.ToMS
			if k.CacheTTL > 0 {
				value.SourceExpiresAtMS = k.Now().Add(k.CacheTTL).UnixMilli()
			}
			cache.mu.Lock()
			if err == nil && k.CacheTTL > 0 && scope.generation == flight.generation {
				include := req.Include
				// Keep bounded previews, never a second copy of full model-history pages.
				if include.EventsPage != nil && (include.EventsPage.Limit > 100 || value.Events == nil) {
					include.EventsPage = nil
				}
				if value.Summary == nil {
					include.Summary = false
				}
				if include.Summary || include.Timeline || include.APIKeyStats || include.EventsPage != nil {
					entries := scope.entries[:0]
					for _, entry := range scope.entries {
						if k.Now().Before(entry.expires) && !usageSourceCovers(include, entry.include) {
							entries = append(entries, entry)
						}
					}
					if len(entries) >= 4 {
						entries = entries[1:]
					}
					scope.entries = append(entries, usageSourceEntry{include: include, value: usageSourceProjection(value, include), expires: k.Now().Add(k.CacheTTL)})
				}
			}
			flight.value, flight.err = value, err
			for index, candidate := range scope.flights {
				if candidate == flight {
					scope.flights = append(scope.flights[:index], scope.flights[index+1:]...)
					break
				}
			}
			close(flight.done)
			cache.mu.Unlock()
		}()
	}
	cache.mu.Unlock()
	select {
	case <-ctx.Done():
		return cpamp.AnalyticsResponse{}, ctx.Err()
	case <-flight.done:
		return usageSourceProjection(flight.value, req.Include), flight.err
	}
}

func usageSourceKeySubset(value cpamp.AnalyticsResponse, req cpamp.AnalyticsRequest) cpamp.AnalyticsResponse {
	value = usageSourceProjection(value, req.Include)
	allowed := make(map[string]bool, len(req.Filters.APIKeyHashes))
	for _, hash := range req.Filters.APIKeyHashes {
		allowed[hash] = true
	}
	stats := make([]cpamp.APIKeyUsageStat, 0, len(allowed))
	for _, stat := range value.APIKeyStats {
		if allowed[stat.APIKeyHash] {
			stats = append(stats, stat)
		}
	}
	value.APIKeyStats = stats
	if value.AGYSource != nil {
		native := usageSourceKeySubset(*value.AGYSource, req)
		value.AGYSource = &native
	}
	return value
}

func (k *Keys) fetchUsageSource(ctx context.Context, req cpamp.AnalyticsRequest) (value cpamp.AnalyticsResponse, err error) {
	defer func() {
		if recover() != nil {
			value = cpamp.AnalyticsResponse{}
			err = errors.New("analytics query failed")
		}
	}()
	value, err = k.CPAMP.Analytics(ctx, req)
	if err == nil && k.AGY != nil {
		value.AGYSource, err = k.readAGYUsage(ctx, req)
	}
	return value, err
}
