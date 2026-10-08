package service

import (
	"errors"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

func TestIdentityCompatibilityUsesCurrentAliasesAndProtectsCodex(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	enabled := map[string]bool{"antigravity": true}
	for model, want := range map[string]bool{"anti-only": true, "anti-public": true, "shared": false, "codex-public": false, "unknown": false} {
		got, err := k.ShouldRemoveGPT5Identity(t.Context(), model, enabled)
		if err != nil || got != want {
			t.Fatalf("%s: got %v, err %v, want %v", model, got, err, want)
		}
	}
	f.excluded["codex"] = append(f.excluded["codex"], "shared")
	if got, err := k.ShouldRemoveGPT5Identity(t.Context(), "shared", enabled); err != nil || !got {
		t.Fatalf("disabled Codex route still claimed the model: %v, %v", got, err)
	}
	// Alias changes must take effect without an old cached provider attribution.
	f.models["codex"] = append(f.models["codex"], cpamp.OAuthModelDefinition{ID: "anti-public"})
	if got, err := k.ShouldRemoveGPT5Identity(t.Context(), "anti-public", enabled); err != nil || got {
		t.Fatalf("new Codex owner ignored: %v, %v", got, err)
	}
	f.aliases["antigravity"] = []cpamp.OAuthModelAlias{{Name: "anti-only", Alias: "renamed", Fork: false}}
	for model, want := range map[string]bool{"renamed": true, "anti-only": false} {
		if got, err := k.ShouldRemoveGPT5Identity(t.Context(), model, enabled); err != nil || got != want {
			t.Fatalf("renamed model %s: %v, %v", model, got, err)
		}
	}
	if got, err := k.ShouldRemoveGPT5Identity(t.Context(), "renamed", nil); err != nil || got {
		t.Fatalf("disabled compatibility changed a request: %v, %v", got, err)
	}
}

func TestIdentityCompatibilitySkipsIncompleteRoutingAndMixedPolicies(t *testing.T) {
	for _, failure := range []string{"auth-files", "discovery", "aliases", "models", "excluded"} {
		t.Run(failure, func(t *testing.T) {
			f := &conflictDiscoveryCPAMP{channelCPAMP: newChannelCPAMP(), discovered: []string{"claude"}, fail: failure}
			if failure == "discovery" {
				f.discoveryErr = errors.New("unavailable")
			}
			if got, err := NewKeys(nil, f).ShouldRemoveGPT5Identity(t.Context(), "anti-only", map[string]bool{"antigravity": true}); err == nil || got {
				t.Fatalf("incomplete route changed instructions: %v, %v", got, err)
			}
		})
	}
	f := newChannelCPAMP()
	f.files = append(f.files, cpamp.AuthFile{Provider: "claude"})
	f.models["claude"] = []cpamp.OAuthModelDefinition{{ID: "anti-only"}}
	k := NewKeys(nil, f)
	if got, err := k.ShouldRemoveGPT5Identity(t.Context(), "anti-only", map[string]bool{"antigravity": true}); err != nil || got {
		t.Fatalf("mixed policies changed a shared name: %v, %v", got, err)
	}
	if got, err := k.ShouldRemoveGPT5Identity(t.Context(), "anti-only", map[string]bool{"antigravity": true, "claude": true}); err != nil || !got {
		t.Fatalf("fully enabled non-Codex pool not handled: %v, %v", got, err)
	}
}
