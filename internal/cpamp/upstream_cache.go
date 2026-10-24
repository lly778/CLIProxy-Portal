package cpamp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type upstreamReadContextKey struct{}

// WithUpstreamReadCache opts a read-only page into shared management reads.
// Writes, verification, conflict checks, quota refresh and other callers retain
// fresh upstream reads unless they explicitly opt in.
func WithUpstreamReadCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, upstreamReadContextKey{}, true)
}

// WithFreshUpstreamReads preserves deadlines and cancellation but disables the
// optional page cache. Credential availability and cooldown checks must not
// reuse the five-minute metadata snapshot used to render account lists.
func WithFreshUpstreamReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, upstreamReadContextKey{}, false)
}

func upstreamReadTTL(path string) time.Duration {
	switch path {
	case pathAuthFiles:
		return 5 * time.Minute
	case pathOAuthExcluded, pathOAuthModelAlias, pathConfigYAML:
		return 30 * time.Minute
	default:
		if strings.HasPrefix(path, pathModelDefinitions+"/") && !strings.Contains(path, "?") {
			return time.Hour
		}
		return 0
	}
}

func upstreamMutation(method, path string) bool {
	path, _, _ = strings.Cut(path, "?")
	return method != http.MethodGet && strings.HasPrefix(path, "/v0/management/") &&
		path != pathAnalytics && path != pathAPICall && path != pathResetQuota
}

type upstreamReadEntry struct {
	body      []byte
	storedAt  time.Time
	expiresAt time.Time
}

type upstreamFlightKey struct {
	path       string
	generation uint64
}

type upstreamReadFlight struct {
	done chan struct{}
	body []byte
	err  error
}

type upstreamReadCache struct {
	mu         sync.Mutex
	generation uint64
	entries    map[string]upstreamReadEntry
	flights    map[upstreamFlightKey]*upstreamReadFlight
	bytes      int
}

// These responses remain private to one client in bounded process memory.
// They can contain CPA configuration, so they must never reach a disk cache,
// diagnostic log, or a client configured for another CPAMP instance.
const (
	upstreamReadMaxEntries = 64
	upstreamReadMaxBytes   = 8 << 20
	upstreamReadMaxEntry   = 1 << 20
)

func (c *Client) InvalidateUpstreamReadCache() {
	cache := &c.upstreamReads
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.generation++
	cache.entries = nil
	cache.bytes = 0
}

func cacheableUpstreamBody(path string, body []byte) bool {
	if len(body) == 0 || len(body) > upstreamReadMaxEntry {
		return false
	}
	if path != pathConfigYAML {
		return json.Valid(body)
	}
	var document yaml.Node
	return yaml.Unmarshal(body, &document) == nil && len(document.Content) == 1 && document.Content[0].Kind == yaml.MappingNode
}

// Expiry and mutations both evict entries. Generations prevent a pre-write
// response from repopulating the cache or being reused by post-write waiters.
func (cache *upstreamReadCache) store(path string, body []byte, ttl time.Duration, now time.Time) {
	if cache.entries == nil {
		cache.entries = make(map[string]upstreamReadEntry)
	}
	for key, entry := range cache.entries {
		if key == path || !now.Before(entry.expiresAt) {
			cache.bytes -= len(entry.body)
			delete(cache.entries, key)
		}
	}
	for len(cache.entries) >= upstreamReadMaxEntries || cache.bytes+len(body) > upstreamReadMaxBytes {
		oldest := ""
		for key, entry := range cache.entries {
			if oldest == "" || entry.storedAt.Before(cache.entries[oldest].storedAt) {
				oldest = key
			}
		}
		cache.bytes -= len(cache.entries[oldest].body)
		delete(cache.entries, oldest)
	}
	cache.entries[path] = upstreamReadEntry{body: bytes.Clone(body), storedAt: now, expiresAt: now.Add(ttl)}
	cache.bytes += len(body)
}

func (c *Client) cachedUpstreamRead(ctx context.Context, path string, ttl time.Duration, fetch func(context.Context) ([]byte, error)) ([]byte, error) {
	cache := &c.upstreamReads
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cache.mu.Lock()
		now := time.Now()
		if entry, ok := cache.entries[path]; ok {
			if now.Before(entry.expiresAt) {
				body := bytes.Clone(entry.body)
				cache.mu.Unlock()
				return body, nil
			}
			cache.bytes -= len(entry.body)
			delete(cache.entries, path)
		}
		key := upstreamFlightKey{path: path, generation: cache.generation}
		if cache.flights == nil {
			cache.flights = make(map[upstreamFlightKey]*upstreamReadFlight)
		}
		flight := cache.flights[key]
		if flight == nil {
			flight = &upstreamReadFlight{done: make(chan struct{})}
			cache.flights[key] = flight
			go func() {
				timeout := c.timeout
				if timeout <= 0 {
					timeout = defaultTimeout
				}
				fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
				defer cancel()
				body, err := fetch(fetchCtx)
				if err == nil {
					err = fetchCtx.Err()
				}
				valid := err == nil && cacheableUpstreamBody(path, body)
				cache.mu.Lock()
				if valid && cache.generation == key.generation {
					cache.store(path, body, ttl, time.Now())
				}
				flight.body, flight.err = body, err
				delete(cache.flights, key)
				close(flight.done)
				cache.mu.Unlock()
			}()
		}
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			cache.mu.Lock()
			current := key.generation == cache.generation
			cache.mu.Unlock()
			if current {
				return bytes.Clone(flight.body), flight.err
			}
		}
	}
}
