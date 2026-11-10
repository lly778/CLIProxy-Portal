package store

import (
	"testing"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/domain"
)

func TestUpstreamPageSettingsBatchIsRequestLocalAndIndependent(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	config := domain.ClientModels{Version: domain.ClientModelsVersion, Enabled: true, DefaultModel: "model", Models: []domain.ClientModel{{ID: "model"}}}
	if err := s.SaveClientModels(ctx, config, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpstreamCardOrder(ctx, "presets", []string{"first"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCodexIdentityCompatibility(ctx, "antigravity", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAGYSettings(ctx, agy.Settings{Models: []agy.Model{{ID: "gemini"}}, Rules: []agy.ModelRule{{ID: "gemini", Enabled: true}}}, 0); err != nil {
		t.Fatal(err)
	}
	page := s.WithUpstreamPageSettings(ctx)
	before, err := s.AGYSettings(page)
	if err != nil || len(before.Models) != 1 || !before.Rules[0].Enabled {
		t.Fatal("batched AGY settings missing")
	}
	updated := before
	updated.Rules = append([]agy.ModelRule(nil), before.Rules...)
	updated.Rules[0].Enabled = false
	if err = s.SaveAGYSettings(ctx, updated, before.Revision); err != nil {
		t.Fatal(err)
	}
	still, err := s.AGYSettings(page)
	if err != nil || !still.Rules[0].Enabled {
		t.Fatal("AGY page snapshot changed mid-request")
	}
	if err := s.SetUpstreamCardOrder(ctx, "presets", []string{"second"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCodexIdentityCompatibility(ctx, "antigravity", false); err != nil {
		t.Fatal(err)
	}
	saved, err := s.ClientModels(page)
	if err != nil || saved.DefaultModel != "model" {
		t.Fatalf("config=%+v err=%v", saved, err)
	}
	order, err := s.UpstreamCardOrder(page, "presets")
	if err != nil || len(order) != 1 || order[0] != "first" {
		t.Fatalf("page settings changed mid-request: %v %v", order, err)
	}
	channels, err := s.EnabledCodexIdentityChannels(page)
	if err != nil || !channels["antigravity"] {
		t.Fatal("batched identity setting missing")
	}
	channels["antigravity"] = false
	channels, err = s.EnabledCodexIdentityChannels(page)
	if err != nil || !channels["antigravity"] {
		t.Fatal("caller modified another channel's shared settings")
	}
	next := s.WithUpstreamPageSettings(ctx)
	fresh, err := s.AGYSettings(next)
	if err != nil || fresh.Rules[0].Enabled {
		t.Fatal("new AGY request reused stale settings")
	}
	order, err = s.UpstreamCardOrder(next, "presets")
	if err != nil || order[0] != "second" {
		t.Fatal("new request reused old database settings")
	}
	channels, err = s.EnabledCodexIdentityChannels(next)
	if err != nil || channels["antigravity"] {
		t.Fatal("new request reused old identity setting")
	}
}
