package service

import (
	"context"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

type selectedModelSource struct {
	cpamp.API
	channels []string
	keys     []string
}

func (f *selectedModelSource) ListOAuthModelDefinitions(_ context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	f.channels = append(f.channels, channel)
	return []cpamp.OAuthModelDefinition{{ID: channel, ThinkingLevels: []string{"low", "high"}}}, nil
}

func (f *selectedModelSource) ListModelsWithAPIKey(_ context.Context, key string) (cpamp.ModelListResponse, error) {
	f.keys = append(f.keys, key)
	return cpamp.ModelListResponse{}, nil
}

func TestModelSourceReadsOnlySelectedChannelAndKeepsKeyChecksLive(t *testing.T) {
	fake := &selectedModelSource{}
	k := NewKeys(nil, fake)
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	models, err := k.upstreamModelDefinitions(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	models[0].ThinkingLevels[0] = "changed"
	models, err = k.upstreamModelDefinitions(ctx, "codex")
	if err != nil || len(fake.channels) != 1 || models[0].ThinkingLevels[0] != "low" {
		t.Fatal("selected definitions not safely reused")
	}
	if _, err := k.upstreamModelDefinitions(cpamp.WithFreshUpstreamReads(ctx), "codex"); err != nil || len(fake.channels) != 2 {
		t.Fatal("fresh validation reused metadata cache")
	}
	if _, err := k.upstreamModelDefinitions(ctx, "antigravity"); err != nil || len(fake.channels) != 3 || fake.channels[2] != "antigravity" {
		t.Fatal("channel scope lost")
	}
	for _, key := range []string{"a", "a", "b"} {
		if _, err := k.modelsWithAPIKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.keys) != 3 {
		t.Fatal("key verification incorrectly cached")
	}
}
