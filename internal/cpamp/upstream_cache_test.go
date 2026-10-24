package cpamp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamPageSharesEndpointReadsAndKeepsVerificationFresh(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	config := "api-keys: []\n"
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		counts[r.URL.Path]++
		switch r.URL.Path {
		case pathAuthFiles:
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex"}]}`))
		case pathOAuthExcluded:
			_, _ = w.Write([]byte(`{"oauth-excluded-models":{"codex":[],"antigravity":[]}}`))
		case pathOAuthModelAlias:
			_, _ = w.Write([]byte(`{"oauth-model-alias":{"codex":[],"antigravity":[]}}`))
		case pathConfigYAML:
			_, _ = w.Write([]byte(config))
		default:
			if strings.HasPrefix(r.URL.Path, pathModelDefinitions+"/") {
				_, _ = w.Write([]byte(`{"models":[{"id":"model"}]}`))
			} else {
				http.NotFound(w, r)
			}
		}
	})
	ctx := WithUpstreamReadCache(t.Context())
	for repeat := 0; repeat < 3; repeat++ {
		for _, channel := range []string{"codex", "antigravity"} {
			if _, err := client.ListAuthFiles(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListOAuthExcludedModels(ctx, channel); err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListOAuthModelAliases(ctx, channel); err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListOAuthModelDefinitions(ctx, channel); err != nil {
				t.Fatal(err)
			}
			body, err := client.GetConfigYAML(ctx)
			if err != nil || string(body) != "api-keys: []\n" {
				t.Fatal("cached configuration was modified", err)
			}
			body[0] = 'x' // The returned copy must not corrupt another panel.
		}
		if _, err := client.ListOAuthChannels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	for path, count := range counts {
		if count != 1 {
			t.Errorf("%s fetched %d times for one shared page", path, count)
		}
	}
	if len(counts) != 6 {
		t.Errorf("expected six distinct management reads, got %d", len(counts))
	}
	config = "api-keys: []\npayload: {}\n"
	mu.Unlock()
	body, err := client.GetConfigYAML(t.Context())
	if err != nil || !bytes.Contains(body, []byte("payload")) {
		t.Fatal("verification read used stale page data", err)
	}
	body, err = client.GetConfigYAML(ctx)
	if err != nil || bytes.Contains(body, []byte("payload")) {
		t.Fatal("ordinary page did not retain its documented cache lifetime", err)
	}
	client.InvalidateUpstreamReadCache()
	body, err = client.GetConfigYAML(ctx)
	if err != nil || !bytes.Contains(body, []byte("payload")) {
		t.Fatal("manual refresh did not replace cached data", err)
	}
}

func TestUpstreamReadExpiryAndErrors(t *testing.T) {
	for path, want := range map[string]time.Duration{
		pathAuthFiles: 5 * time.Minute, pathOAuthExcluded: 30 * time.Minute,
		pathOAuthModelAlias: 30 * time.Minute, pathConfigYAML: 30 * time.Minute,
		pathModelDefinitions + "/codex": time.Hour, pathAnalytics: 0,
		pathAuthFiles + "/models?name=x": 0,
	} {
		if upstreamReadTTL(path) != want {
			t.Errorf("unexpected cache duration for %s", path)
		}
	}
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case 2:
			_, _ = w.Write([]byte(`invalid JSON`))
		default:
			_, _ = w.Write([]byte(`{"files":[]}`))
		}
	})
	ctx := WithUpstreamReadCache(t.Context())
	for i := 0; i < 2; i++ {
		if _, err := client.ListAuthFiles(ctx); err == nil {
			t.Fatal("invalid response unexpectedly succeeded")
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := client.ListAuthFiles(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("failed responses were cached or successful response was not reused")
	}
	cache := &client.upstreamReads
	cache.mu.Lock()
	entry := cache.entries[pathAuthFiles]
	entry.expiresAt = time.Now().Add(-time.Second)
	cache.entries[pathAuthFiles] = entry
	cache.mu.Unlock()
	if _, err := client.ListAuthFiles(ctx); err != nil || calls.Load() != 4 {
		t.Fatal("expired credentials were reused", err)
	}
}

func TestUpstreamWritesInvalidateEvenAfterPartialFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			var changed atomic.Bool
			client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					changed.Store(true)
					if failed {
						http.Error(w, "reply lost after write", http.StatusBadGateway)
					} else {
						_, _ = w.Write([]byte(`{}`))
					}
					return
				}
				if changed.Load() {
					_, _ = w.Write([]byte(`{"oauth-excluded-models":{"codex":["model"]}}`))
				} else {
					_, _ = w.Write([]byte(`{"oauth-excluded-models":{"codex":[]}}`))
				}
			})
			ctx := WithUpstreamReadCache(t.Context())
			if _, err := client.ListOAuthExcludedModels(ctx, "codex"); err != nil {
				t.Fatal(err)
			}
			err := client.SetOAuthExcludedModels(t.Context(), "codex", []string{"model"})
			if (err != nil) != failed {
				t.Fatal("unexpected write result", err)
			}
			models, err := client.ListOAuthExcludedModels(ctx, "codex")
			if err != nil || len(models) != 1 || models[0] != "model" {
				t.Fatal("write left old page data cached", err, models)
			}
		})
	}
	if upstreamMutation(http.MethodPost, pathAnalytics) || upstreamMutation(http.MethodPost, pathAPICall) || upstreamMutation(http.MethodPost, pathResetQuota) {
		t.Fatal("read-only analytics or quota polling invalidates configuration caches")
	}
}

func TestUpstreamReadCoalescingSurvivesCancelledPage(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"files":[]}`))
	})
	defer once.Do(func() { close(release) })
	ctx, cancel := context.WithCancel(WithUpstreamReadCache(t.Context()))
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := client.ListAuthFiles(ctx); first <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("shared request did not start")
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled waiter did not return", err)
	}
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		go func() { _, err := client.ListAuthFiles(WithUpstreamReadCache(t.Context())); results <- err }()
	}
	once.Do(func() { close(release) })
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal("one cancelled waiter aborted another page", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent page loads duplicated the management request", calls.Load())
	}
}

func TestUpstreamInvalidationFencesOlderInFlightRead(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			_, _ = w.Write([]byte(`{"oauth-excluded-models":{"codex":["old"]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"oauth-excluded-models":{"codex":["new"]}}`))
	})
	defer once.Do(func() { close(release) })
	ctx := WithUpstreamReadCache(t.Context())
	result := make(chan []string, 1)
	go func() { models, _ := client.ListOAuthExcludedModels(ctx, "codex"); result <- models }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not start")
	}
	client.InvalidateUpstreamReadCache()
	models, err := client.ListOAuthExcludedModels(ctx, "codex")
	if err != nil || len(models) != 1 || models[0] != "new" {
		t.Fatal("refresh joined the pre-invalidation request", err, models)
	}
	once.Do(func() { close(release) })
	select {
	case models := <-result:
		if len(models) != 1 || models[0] != "new" {
			t.Fatal("old response survived invalidation", models)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old waiter did not retry after invalidation")
	}
	if calls.Load() != 2 {
		t.Fatal("invalidation fetched an unexpected number of snapshots", calls.Load())
	}
}

func TestUpstreamCacheSizeAndInstanceIsolation(t *testing.T) {
	cache := upstreamReadCache{}
	now := time.Now()
	for i := 0; i <= upstreamReadMaxEntries; i++ {
		cache.store(fmt.Sprint(i), []byte(`{}`), time.Hour, now.Add(time.Duration(i)*time.Second))
	}
	if len(cache.entries) != upstreamReadMaxEntries || cache.entries["0"].body != nil {
		t.Fatal("cache did not evict the oldest entry at its count limit")
	}
	for i := 0; i < 10; i++ {
		cache.store(fmt.Sprint(i), bytes.Repeat([]byte("x"), upstreamReadMaxEntry), time.Hour, now.Add(time.Duration(i)*time.Second))
	}
	if cache.bytes > upstreamReadMaxBytes {
		t.Fatal("cache exceeded its memory budget")
	}
	for _, label := range []string{"first", "second"} {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"oauth-excluded-models":{"codex":[%q]}}`, label)
		})
		models, err := client.ListOAuthExcludedModels(WithUpstreamReadCache(t.Context()), "codex")
		if err != nil || len(models) != 1 || models[0] != label {
			t.Fatal("cache crossed CPAMP client instances", err, models)
		}
	}
}
