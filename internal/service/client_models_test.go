package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

type clientCatalogAPI struct {
	cpamp.API
	keys []string
}

func (f *clientCatalogAPI) ListAPIKeys(context.Context) ([]string, error) { return f.keys, nil }
func (f *clientCatalogAPI) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	return []cpamp.AuthFile{{Name: "codex.json", Provider: "codex", Disabled: true}, {Name: "antigravity.json", Provider: "antigravity", Unavailable: true}}, nil
}
func (f *clientCatalogAPI) ListOAuthModelDefinitions(_ context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	return []cpamp.OAuthModelDefinition{{ID: channel + "-offline", ThinkingLevels: []string{"low", "medium", "high"}}, {ID: "hidden-original", ThinkingLevels: []string{"low", "medium", "high", "xhigh"}}, {ID: "custom-source", ThinkingLevels: []string{"low", "high"}}}, nil
}
func (f *clientCatalogAPI) ListOAuthModelAliases(context.Context, string) ([]cpamp.OAuthModelAlias, error) {
	return []cpamp.OAuthModelAlias{{Name: "hidden-original", Alias: "user-alias"}}, nil
}

func (f *clientCatalogAPI) ListAuthFileModels(_ context.Context, file cpamp.AuthFile) ([]cpamp.Model, error) {
	return []cpamp.Model{{ID: file.Provider + "-registered-disabled"}}, nil
}
func (f *clientCatalogAPI) GetConfigYAML(context.Context) ([]byte, error) {
	return []byte("openai-compatibility:\n  - name: custom\n    prefix: team\n    api-key-entries:\n      - api-key: DO-NOT-EXPOSE\n    models:\n      - name: custom-source\n        alias: custom-alias\n"), nil
}

func TestClientCatalogIncludesDisabledModelsAliasesAndCustomModels(t *testing.T) {
	k := &Keys{CPAMP: &clientCatalogAPI{}, Now: time.Now}
	models, err := k.ClientModelCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.ID] = true
	}
	for _, id := range []string{"codex-offline", "antigravity-offline", "codex-registered-disabled", "hidden-original", "user-alias", "custom-source", "custom-alias", "team/custom-alias"} {
		if !seen[id] {
			t.Fatalf("missing %s: %+v", id, models)
		}
	}
	for _, model := range models {
		if model.ID == "user-alias" && strings.Join(model.SupportedReasoningLevels, ",") != "low,medium,high,xhigh" {
			t.Fatalf("OAuth alias lost levels: %+v", model)
		}
		if model.ID == "team/custom-alias" && strings.Join(model.SupportedReasoningLevels, ",") != "low,high" {
			t.Fatalf("prefixed API alias lost levels: %+v", model)
		}
	}
	payload, _ := json.Marshal(models)
	if strings.Contains(string(payload), "DO-NOT-EXPOSE") {
		t.Fatal("leaked credential")
	}
}

func TestPublishedClientModelsNeedNoUserOrKeyAndIgnoreModelAvailability(t *testing.T) {
	_, st := accountsForTest(t)
	ctx := t.Context()
	k := NewKeys(st, &clientCatalogAPI{})
	if _, err := k.PublishedClientModels(ctx); !errors.Is(err, ErrClientModelsUnavailable) {
		t.Fatalf("unconfigured: %v", err)
	}
	// Legacy saved configurations become available without a separate publish toggle.
	publication := domain.ClientModels{Version: 2, Enabled: false, DefaultModel: "disabled", Models: []domain.ClientModel{{ID: "disabled", DefaultReasoning: "xhigh"}}}
	if err := st.SaveClientModels(ctx, publication, 0); err != nil {
		t.Fatal(err)
	}
	got, err := k.PublishedClientModels(ctx)
	if err != nil || !got.Enabled || len(got.Models) != 1 || got.DefaultModel != "disabled" || got.Models[0].DefaultReasoning != "xhigh" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
