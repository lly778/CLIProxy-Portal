package service

import (
	"context"
	"errors"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

func TestValidateOAuthAliasNamesRespectsModelStatus(t *testing.T) {
	models := []OAuthModelSetting{
		{ID: "gpt-5.6-luna", Enabled: false},
		{ID: "gpt-6-luna", Enabled: true},
	}
	aliases := []cpamp.OAuthModelAlias{{Name: "gpt-6-luna", Alias: "gpt-5.6-luna", Fork: true}}
	if err := validateOAuthAliasNames(models, aliases); err != nil {
		t.Fatalf("disabled model's original name should be available: %v", err)
	}

	models[0].Enabled = true
	if err := validateOAuthAliasNames(models, aliases); err == nil {
		t.Fatal("enabled model's original name must conflict with another model's alias")
	}

	aliases = append(aliases, cpamp.OAuthModelAlias{Name: "gpt-5.6-luna", Alias: "legacy-luna"})
	if err := validateOAuthAliasNames(models, aliases); err != nil {
		t.Fatalf("renaming the other model should free its original name: %v", err)
	}

	aliases[1].Fork = true
	if err := validateOAuthAliasNames(models, aliases); err == nil {
		t.Fatal("forking the other model must reclaim its original name")
	}
}

type visibleModelsCPAMP struct {
	cpamp.API
	aliases  []cpamp.OAuthModelAlias
	aliasErr error
}

func (*visibleModelsCPAMP) ListAPIKeys(context.Context) ([]string, error) {
	return []string{"sk-test"}, nil
}

func (*visibleModelsCPAMP) ListModelsWithAPIKey(context.Context, string) (cpamp.ModelListResponse, error) {
	return cpamp.ModelListResponse{Data: []cpamp.Model{
		{ID: "deepseek-flash"}, {ID: "gpt-5.6-luna"}, {ID: "gpt-5.6-sol"}, {ID: "gpt-6-luna"}, {ID: "gpt-6-sol"},
	}}, nil
}

func (f *visibleModelsCPAMP) ListOAuthModelAliases(context.Context, string) ([]cpamp.OAuthModelAlias, error) {
	return f.aliases, f.aliasErr
}

func (*visibleModelsCPAMP) ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error) {
	return []cpamp.OAuthModelDefinition{{ID: "gpt-5.6-luna"}, {ID: "gpt-5.6-sol"}, {ID: "gpt-6-luna"}, {ID: "gpt-6-sol"}}, nil
}

func (*visibleModelsCPAMP) ListOAuthExcludedModels(context.Context, string) ([]string, error) {
	return []string{"gpt-5.6-luna", "gpt-5.6-sol"}, nil
}

func TestVisibleModelsHidesAliasesButKeepsEnabledRealNames(t *testing.T) {
	fake := &visibleModelsCPAMP{aliases: []cpamp.OAuthModelAlias{
		{Name: "gpt-6-luna", Alias: "gpt-5.6-luna", Fork: true},
		{Name: "gpt-6-sol", Alias: "gpt-5.6-sol", Fork: true},
		{Name: "gpt-6-luna", Alias: "gpt-6-sol", Fork: true},
	}}
	keys := NewKeys(nil, fake)
	models, err := keys.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"deepseek-flash", "gpt-6-luna", "gpt-6-sol"}
	if len(models) != len(want) {
		t.Fatalf("visible models = %+v", models)
	}
	for i, model := range models {
		if model.ID != want[i] {
			t.Fatalf("visible model %d = %q, want %q", i, model.ID, want[i])
		}
	}
	fake.aliasErr = errors.New("management unavailable")
	if models, err := keys.Models(context.Background()); err == nil || len(models) != 0 {
		t.Fatalf("unfiltered models were exposed after alias lookup failed: %+v, %v", models, err)
	}
	fake.aliasErr = nil
	fake.aliases = nil
	if models, err := keys.Models(context.Background()); err != nil || len(models) != 5 {
		t.Fatalf("models without aliases = %+v, %v", models, err)
	}
}
