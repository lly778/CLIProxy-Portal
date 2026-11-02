package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

type countingClientCatalogAPI struct {
	*clientCatalogAPI
	reads      atomic.Int32
	generation atomic.Uint64
	gate       <-chan struct{}
	started    chan struct{}
	once       sync.Once
}

func (f *countingClientCatalogAPI) UpstreamReadGeneration() uint64 { return f.generation.Load() }
func (f *countingClientCatalogAPI) InvalidateUpstreamReadCache()   { f.generation.Add(1) }
func (f *countingClientCatalogAPI) ListOAuthModelDefinitions(ctx context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	f.reads.Add(1)
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.clientCatalogAPI.ListOAuthModelDefinitions(ctx, channel)
}

func TestClientCatalogCoalescesAndFollowsManagementInvalidation(t *testing.T) {
	api := &countingClientCatalogAPI{clientCatalogAPI: &clientCatalogAPI{}}
	k := NewKeys(nil, api)
	var group sync.WaitGroup
	failures := make(chan error, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := k.ClientModelCatalog(t.Context())
			if err != nil {
				failures <- err
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if reads := api.reads.Load(); reads != 2 {
		t.Fatalf("duplicate catalog reads: %d", reads)
	}
	first, err := k.ClientModelCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first[0].ID = "changed by caller"
	if len(first[0].SupportedReasoningLevels) != 0 {
		first[0].SupportedReasoningLevels[0] = "changed by caller"
	}
	second, err := k.ClientModelCatalog(t.Context())
	if err != nil || second[0].ID == first[0].ID {
		t.Fatalf("cache aliased: %v", err)
	}
	if len(second[0].SupportedReasoningLevels) != 0 && second[0].SupportedReasoningLevels[0] == "changed by caller" {
		t.Fatal("cached reasoning levels were mutated by caller")
	}
	api.InvalidateUpstreamReadCache()
	if _, err := k.ClientModelCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reads := api.reads.Load(); reads != 4 {
		t.Fatalf("upstream mutation did not invalidate aggregate: %d", reads)
	}
	k.InvalidateUpstreamReadCache()
	if _, err := k.ClientModelCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reads := api.reads.Load(); reads != 6 {
		t.Fatalf("page refresh did not invalidate: %d", reads)
	}
}

func TestClientCatalogCanceledWaiterDoesNotCancelSharedFetch(t *testing.T) {
	gate := make(chan struct{})
	api := &countingClientCatalogAPI{clientCatalogAPI: &clientCatalogAPI{}, gate: gate, started: make(chan struct{})}
	k := NewKeys(nil, api)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := k.ClientModelCatalog(ctx); result <- err }()
	<-api.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	close(gate)
	if _, err := k.ClientModelCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reads := api.reads.Load(); reads != 2 {
		t.Fatalf("canceled waiter restarted shared fetch: %d", reads)
	}
}
