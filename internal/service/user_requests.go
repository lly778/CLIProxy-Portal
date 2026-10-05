package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/security"
)

// UserRequests reads only recent metadata for keys owned by this user, including
// revoked keys. Full usage statistics and model-history pagination belong to Usage.
func (k *Keys) UserRequests(ctx context.Context, userID string, from time.Time, limit int) (cpamp.EventsResponse, error) {
	return k.userRequests(ctx, userID, from, limit, false)
}

func (k *Keys) RefreshUserRequests(ctx context.Context, userID string, from time.Time, limit int) (cpamp.EventsResponse, error) {
	return k.userRequests(ctx, userID, from, limit, true)
}

func (k *Keys) userRequests(ctx context.Context, userID string, from time.Time, limit int, refresh bool) (cpamp.EventsResponse, error) {
	limit = max(1, min(100, limit))
	now := k.Now()
	if from.IsZero() || !from.Before(now) {
		from = now.AddDate(-10, 0, 0)
	}
	hashes, err := k.hashesForUser(ctx, userID)
	if err != nil {
		return cpamp.EventsResponse{}, err
	}
	// Never issue an unfiltered analytics request for users without keys.
	if len(hashes) == 0 {
		return cpamp.EventsResponse{Items: []cpamp.EventRow{}}, nil
	}
	// Let CacheTTL govern reuse instead of invalidating at every minute boundary.
	// The key set is included so newly issued keys immediately invalidate the cache.
	cacheKey := fmt.Sprintf("user-requests:%s:%d:%d:%s", userID, from.UnixMilli(), limit, security.SHA256(strings.Join(hashes, ",")))
	if !refresh {
		if cached, ok := k.cached(cacheKey); ok && cached.Events != nil {
			return *cached.Events, nil
		}
	}
	req := cpamp.AnalyticsRequest{
		FromMS: from.UnixMilli(), ToMS: now.UnixMilli(), NowMS: now.UnixMilli(), TimeZone: "Asia/Shanghai",
		Filters: cpamp.AnalyticsFilters{APIKeyHashes: hashes},
		Include: cpamp.AnalyticsInclude{EventsPage: &cpamp.EventsPage{Limit: limit}},
	}
	value, err := k.CPAMP.Analytics(ctx, req)
	if err != nil {
		return cpamp.EventsResponse{}, err
	}
	if value.Events == nil {
		return cpamp.EventsResponse{}, errors.New("cpamp user request events missing")
	}
	allowed := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		allowed[hash] = true
	}
	page := *value.Events
	page.Items = append([]cpamp.EventRow(nil), page.Items...)
	for _, event := range page.Items {
		if !allowed[event.APIKeyHash] {
			return cpamp.EventsResponse{}, errors.New("cpamp user request events outside key scope")
		}
	}
	sort.SliceStable(page.Items, func(i, j int) bool { return page.Items[i].TimestampMS > page.Items[j].TimestampMS })
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
	}
	k.putCache(cacheKey, cpamp.AnalyticsResponse{Events: &page})
	return page, nil
}
