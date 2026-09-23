package service

import (
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
