package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

func TestUpstreamViewsCoalesceCloneAndBypassFreshReads(t *testing.T) {
	k := &Keys{}
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	var reads atomic.Int32
	read := func(context.Context) (upstreamModelView, error) {
		reads.Add(1)
		return upstreamModelView{Models: []OAuthModelSetting{{ID: "model", ThinkingLevels: []string{"low", "high"}}}}, nil
	}
	var group sync.WaitGroup
	for range 24 {
		group.Add(1)
		go func() {
			defer group.Done()
			value, err := upstreamView(ctx, k, "models:codex", read, cloneUpstreamModels)
			if err != nil {
				t.Error(err)
				return
			}
			value.Models[0].ThinkingLevels[0] = "changed"
		}()
	}
	group.Wait()
	value, err := upstreamView(ctx, k, "models:codex", read, cloneUpstreamModels)
	if err != nil || reads.Load() != 1 || value.Models[0].ThinkingLevels[0] != "low" {
		t.Fatalf("reads=%d value=%+v err=%v", reads.Load(), value, err)
	}
	_, err = upstreamView(cpamp.WithFreshUpstreamReads(ctx), k, "models:codex", read, cloneUpstreamModels)
	if err != nil || reads.Load() != 2 {
		t.Fatal("fresh validation used derived cache")
	}
	k.InvalidateUpstreamReadCache()
	_, err = upstreamView(ctx, k, "models:codex", read, cloneUpstreamModels)
	if err != nil || reads.Load() != 3 {
		t.Fatal("explicit refresh did not invalidate derived cache")
	}
}

func TestUpstreamViewsFenceInFlightRefreshAndWaiterCancellation(t *testing.T) {
	k := &Keys{}
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	read := func(ctx context.Context) (int32, error) {
		count := reads.Add(1)
		if count == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return count, nil
	}
	clone := func(value int32) int32 { return value }
	waiter, cancel := context.WithCancel(ctx)
	canceled := make(chan error, 1)
	go func() { _, err := upstreamView(waiter, k, "sample", read, clone); canceled <- err }()
	<-started
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	k.InvalidateUpstreamReadCache()
	value, err := upstreamView(ctx, k, "sample", read, clone)
	close(release)
	if err != nil || value != 2 {
		t.Fatalf("stale flight reused: %d %v", value, err)
	}
	value, err = upstreamView(ctx, k, "sample", read, clone)
	if err != nil || value != 2 {
		t.Fatalf("old flight repopulated current cache: %d %v", value, err)
	}
}

func TestUpstreamViewErrorsAndExpirationAreNotReused(t *testing.T) {
	k := &Keys{}
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	reads := 0
	read := func(context.Context) (string, error) { reads++; return "", errors.New("offline") }
	clone := func(value string) string { return value }
	for range 2 {
		_, _ = upstreamView(ctx, k, "failed", read, clone)
	}
	if reads != 2 {
		t.Fatal("read error was cached")
	}
	k.upstreamViews.entries = map[string]upstreamViewEntry{"expired": {value: "old", expires: time.Now().Add(-time.Second)}}
	value, err := upstreamView(ctx, k, "expired", func(context.Context) (string, error) { return "new", nil }, clone)
	if err != nil || value != "new" {
		t.Fatalf("expired result=%s err=%v", value, err)
	}
}

type reasoningViewAPI struct {
	cpamp.API
	reads atomic.Int32
}

func (f *reasoningViewAPI) GetConfigYAML(context.Context) ([]byte, error) {
	f.reads.Add(1)
	return []byte("api-keys: []\n"), nil
}

func TestUpstreamReasoningReadSharedAcrossChannels(t *testing.T) {
	api := &reasoningViewAPI{}
	k := &Keys{CPAMP: api}
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	for _, channel := range []string{"codex", "antigravity", "codex"} {
		caps, _, err := k.OAuthReasoningCaps(ctx, channel)
		if err != nil {
			t.Fatal(err)
		}
		caps["caller-change"] = "high"
	}
	caps, _, err := k.OAuthReasoningCaps(ctx)
	if err != nil || api.reads.Load() != 1 || caps["caller-change"] != "" {
		t.Fatalf("reads=%d caps=%v err=%v", api.reads.Load(), caps, err)
	}
}

type upstreamViewGenerationAPI struct {
	cpamp.API
	generation atomic.Uint64
	deadline   time.Time
}

func (f *upstreamViewGenerationAPI) UpstreamReadGeneration() uint64  { return f.generation.Load() }
func (f *upstreamViewGenerationAPI) UpstreamReadDeadline() time.Time { return f.deadline }

func TestUpstreamViewTracksRawDeadlineAndGeneration(t *testing.T) {
	api := &upstreamViewGenerationAPI{deadline: time.Now().Add(time.Minute)}
	k := &Keys{CPAMP: api}
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	reads := 0
	read := func(context.Context) (int, error) { reads++; return reads, nil }
	clone := func(value int) int { return value }
	_, err := upstreamView(ctx, k, "sample", read, clone)
	if err != nil {
		t.Fatal(err)
	}
	k.upstreamViews.mu.Lock()
	expires := k.upstreamViews.entries["sample"].expires
	k.upstreamViews.mu.Unlock()
	if expires.After(api.deadline) {
		t.Fatal("derived view extended original response freshness")
	}
	api.generation.Add(1)
	value, err := upstreamView(ctx, k, "sample", read, clone)
	if err != nil || value != 2 {
		t.Fatalf("upstream mutation did not invalidate view: %d %v", value, err)
	}
}
