package service

import (
	"context"
	"sync"
	"time"

	"cliproxy-portal/internal/cpamp"
)

// Derived read results contain only sanitized metadata; authentication and writes bypass them.
type upstreamViewCache struct {
	mu         sync.Mutex
	generation uint64
	entries    map[string]upstreamViewEntry
	flights    map[upstreamViewKey]*upstreamViewFlight
}

type upstreamViewKey struct {
	name            string
	local, upstream uint64
}
type upstreamViewEntry struct {
	value           any
	expires         time.Time
	local, upstream uint64
}
type upstreamViewFlight struct {
	done    chan struct{}
	value   any
	err     error
	expires time.Time
}

type upstreamViewDeadlineKey struct{}
type upstreamViewDeadline struct {
	mu      sync.Mutex
	expires time.Time
}

func recordUpstreamDeadline(ctx context.Context, expires time.Time) {
	if value, ok := ctx.Value(upstreamViewDeadlineKey{}).(*upstreamViewDeadline); ok && !expires.IsZero() {
		value.mu.Lock()
		if value.expires.IsZero() || expires.Before(value.expires) {
			value.expires = expires
		}
		value.mu.Unlock()
	}
}

func upstreamView[T any](ctx context.Context, k *Keys, name string, read func(context.Context) (T, error), clone func(T) T) (T, error) {
	if !cpamp.UpstreamReadCacheEnabled(ctx) {
		return read(ctx)
	}
	cache := &k.upstreamViews
	for {
		var zero T
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		cache.mu.Lock()
		key := upstreamViewKey{name: name, local: cache.generation, upstream: k.upstreamReadGeneration()}
		if entry, ok := cache.entries[name]; ok && entry.local == key.local && entry.upstream == key.upstream && time.Now().Before(entry.expires) {
			cache.mu.Unlock()
			recordUpstreamDeadline(ctx, entry.expires)
			return clone(entry.value.(T)), nil
		}
		if cache.flights == nil {
			cache.flights = make(map[upstreamViewKey]*upstreamViewFlight)
		}
		flight := cache.flights[key]
		if flight == nil {
			flight = &upstreamViewFlight{done: make(chan struct{})}
			cache.flights[key] = flight
			go func() {
				fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
				defer cancel()
				dependencies := &upstreamViewDeadline{}
				fetchCtx = context.WithValue(fetchCtx, upstreamViewDeadlineKey{}, dependencies)
				value, err := read(fetchCtx)
				if err == nil {
					err = fetchCtx.Err()
				}
				expires := time.Now().Add(5 * time.Minute)
				dependencies.mu.Lock()
				if !dependencies.expires.IsZero() && dependencies.expires.Before(expires) {
					expires = dependencies.expires
				}
				dependencies.mu.Unlock()
				if reader, ok := k.CPAMP.(interface{ UpstreamReadDeadline() time.Time }); ok {
					if deadline := reader.UpstreamReadDeadline(); !deadline.IsZero() && deadline.Before(expires) {
						expires = deadline
					}
				}
				cache.mu.Lock()
				if err == nil && key.local == cache.generation && key.upstream == k.upstreamReadGeneration() && time.Now().Before(expires) {
					if cache.entries == nil {
						cache.entries = make(map[string]upstreamViewEntry)
					}
					for id, entry := range cache.entries {
						if !time.Now().Before(entry.expires) || entry.local != key.local || entry.upstream != key.upstream {
							delete(cache.entries, id)
						}
					}
					// Bound the number of cached channel/component combinations.
					if len(cache.entries) >= 128 {
						cache.entries = make(map[string]upstreamViewEntry)
					}
					cache.entries[name] = upstreamViewEntry{value: clone(value), expires: expires, local: key.local, upstream: key.upstream}
				}
				flight.value, flight.err, flight.expires = value, err, expires
				delete(cache.flights, key)
				close(flight.done)
				cache.mu.Unlock()
			}()
		}
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-flight.done:
		}
		cache.mu.Lock()
		current := key.local == cache.generation && key.upstream == k.upstreamReadGeneration()
		cache.mu.Unlock()
		if !current {
			continue
		}
		if flight.err != nil {
			return zero, flight.err
		}
		recordUpstreamDeadline(ctx, flight.expires)
		return clone(flight.value.(T)), nil
	}
}

type upstreamModelView struct {
	Models    []OAuthModelSetting
	Wildcards []string
}

func cloneUpstreamModels(value upstreamModelView) upstreamModelView {
	value.Models = append([]OAuthModelSetting(nil), value.Models...)
	for index := range value.Models {
		value.Models[index].ThinkingLevels = append([]string(nil), value.Models[index].ThinkingLevels...)
	}
	value.Wildcards = append([]string(nil), value.Wildcards...)
	return value
}

type upstreamAliasView struct {
	Aliases  []cpamp.OAuthModelAlias
	Revision string
}

func cloneUpstreamAliases(value upstreamAliasView) upstreamAliasView {
	value.Aliases = append([]cpamp.OAuthModelAlias(nil), value.Aliases...)
	return value
}

func cloneOAuthSnapshot(value OAuthPresetSnapshot) OAuthPresetSnapshot {
	value.Channels = append([]OAuthChannelPreset(nil), value.Channels...)
	for index := range value.Channels {
		entry := &value.Channels[index]
		entry.Models = append([]OAuthPresetModel(nil), entry.Models...)
		entry.Aliases = append([]cpamp.OAuthModelAlias(nil), entry.Aliases...)
		caps := make(map[string]string, len(entry.ReasoningCaps))
		for id, cap := range entry.ReasoningCaps {
			caps[id] = cap
		}
		entry.ReasoningCaps = caps
	}
	return value
}
