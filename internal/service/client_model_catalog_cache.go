package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"cliproxy-portal/internal/cpamp"
)

type clientCatalogCache struct {
	mu                 sync.Mutex
	value              []ClientCatalogModel
	expires            time.Time
	upstreamGeneration uint64
	localGeneration    uint64
	flight             *clientCatalogFlight
}

type clientCatalogFlight struct {
	done                                chan struct{}
	upstreamGeneration, localGeneration uint64
	value                               []ClientCatalogModel
	err                                 error
}

func (k *Keys) upstreamReadGeneration() uint64 {
	if reader, ok := k.CPAMP.(interface{ UpstreamReadGeneration() uint64 }); ok {
		return reader.UpstreamReadGeneration()
	}
	return 0
}

// Keep one bounded sanitized catalog for the OAuth model-definition TTL.
// Requests share in-flight work, independently of a waiter's lifetime.
// The CPAMP generation also invalidates it after routing/configuration writes.
func (k *Keys) ClientModelCatalog(ctx context.Context) ([]ClientCatalogModel, error) {
	cache := &k.clientCatalog
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cache.mu.Lock()
		generation := k.upstreamReadGeneration()
		if cache.value != nil && time.Now().Before(cache.expires) && cache.upstreamGeneration == generation {
			result := cloneClientCatalog(cache.value)
			cache.mu.Unlock()
			return result, nil
		}
		flight := cache.flight
		if flight == nil {
			flight = &clientCatalogFlight{done: make(chan struct{}), upstreamGeneration: generation, localGeneration: cache.localGeneration}
			cache.flight = flight
			go func(f *clientCatalogFlight) {
				fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
				defer cancel()
				value, err := k.readClientModelCatalog(fetchCtx)
				if err == nil {
					err = fetchCtx.Err()
				}
				if err == nil && len(value) > 10000 {
					err = errors.New("模型目录超过安全数量上限")
				}
				cache.mu.Lock()
				f.value, f.err = value, err
				if err == nil && f.localGeneration == cache.localGeneration && f.upstreamGeneration == k.upstreamReadGeneration() {
					cache.value = cloneClientCatalog(value)
					cache.expires = time.Now().Add(cpamp.OAuthModelDefinitionsCacheTTL)
					cache.upstreamGeneration = f.upstreamGeneration
				}
				cache.flight = nil
				close(f.done)
				cache.mu.Unlock()
			}(flight)
		}
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
		}
		cache.mu.Lock()
		current := flight.localGeneration == cache.localGeneration && flight.upstreamGeneration == k.upstreamReadGeneration()
		cache.mu.Unlock()
		if !current {
			continue
		}
		if flight.err != nil {
			return nil, flight.err
		}
		return cloneClientCatalog(flight.value), nil
	}
}
