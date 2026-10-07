package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

func TestVisibleModelsFiltersAliasesAcrossOAuthChannels(t *testing.T) {
	f := newChannelCPAMP()
	f.models = map[string][]cpamp.OAuthModelDefinition{
		"codex":       {{ID: "gpt-6.1-sol"}, {ID: "gpt-5.6-luna"}, {ID: "gpt-6-luna"}},
		"antigravity": {{ID: "gemini-3.8-flash-high"}},
	}
	f.excluded["codex"] = []string{"gpt-5.6-luna", "gpt-6-luna"}
	f.aliases = map[string][]cpamp.OAuthModelAlias{
		"codex":       {{Name: "gpt-6.1-sol", Alias: "gpt-5.6-sol", Fork: true}, {Name: "gpt-6.1-sol", Alias: "gpt-6-sol", Fork: true}},
		"antigravity": {{Name: "gemini-3.8-flash-high", Alias: " GPT-5.6-LUNA ", Fork: true}, {Name: "gemini-3.8-flash-high", Alias: "gpt-6-luna", Fork: true}},
	}
	k := NewKeys(nil, f)
	input := []cpamp.Model{{ID: "deepseek-flash"}, {ID: "gemini-3.8-flash-high"}, {ID: "gpt-5.6-luna"}, {ID: "gpt-6-luna"}, {ID: "gpt-6.1-sol"}, {ID: "gpt-5.6-sol"}, {ID: " GPT-6-SOL "}}
	got, err := k.VisibleModels(t.Context(), input)
	want := []cpamp.Model{input[0], input[1], input[4]}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("visible=%+v err=%v", got, err)
	}
	// A real name in another channel overrides an alias, even when that channel
	// has no aliases of its own. The next read must observe changed settings.
	f.aliases["codex"] = nil
	f.excluded["codex"] = []string{"gpt-5.6-luna"}
	got, err = k.VisibleModels(t.Context(), input[:5])
	want = []cpamp.Model{input[0], input[1], input[3], input[4]}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("enabled real name not retained: %+v err=%v", got, err)
	}
	// Wildcards still disable real models and cannot free their aliases from filtering.
	f.excluded["codex"] = []string{"gpt-*-luna"}
	got, err = k.VisibleModels(t.Context(), input[:5])
	if err != nil || !reflect.DeepEqual(got, []cpamp.Model{input[0], input[1], input[4]}) {
		t.Fatalf("wildcard state ignored: %+v err=%v", got, err)
	}
}

func TestVisibleModelsDiscoversConfiguredOnlyAliasesAndFailsClosed(t *testing.T) {
	for _, failure := range []string{"", "auth-files", "discovery", "aliases", "models", "excluded"} {
		t.Run(failure, func(t *testing.T) {
			f := &conflictDiscoveryCPAMP{channelCPAMP: newChannelCPAMP(), discovered: []string{"claude"}, fail: failure}
			f.models["claude"] = []cpamp.OAuthModelDefinition{{ID: "claude-real"}}
			f.aliases["claude"] = []cpamp.OAuthModelAlias{{Name: "claude-real", Alias: "claude-alias", Fork: true}, {Name: "claude-real", Alias: " CLAUDE-REAL "}}
			if failure == "discovery" {
				f.discoveryErr = errors.New("unavailable")
			}
			got, err := NewKeys(nil, f).VisibleModels(t.Context(), []cpamp.Model{{ID: "claude-real"}, {ID: "claude-alias"}})
			if failure != "" {
				if err == nil || len(got) != 0 {
					t.Fatalf("returned unfiltered models: %+v err=%v", got, err)
				}
			} else if err != nil || !reflect.DeepEqual(got, []cpamp.Model{{ID: "claude-real"}}) {
				t.Fatalf("configured-only aliases not filtered: %+v err=%v", got, err)
			}
		})
	}
}

type recoveredAliasCPAMP struct{ *channelCPAMP }

func (f *recoveredAliasCPAMP) ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error) {
	return []cpamp.Model{{ID: "anti-public"}, {ID: "anti-only"}}, nil
}

func TestVisibleModelsAlsoFiltersRecoveredCredentialAliases(t *testing.T) {
	f := &recoveredAliasCPAMP{newChannelCPAMP()}
	f.files[1] = cpamp.AuthFile{Provider: "antigravity", AuthIndex: "opaque", Status: "active", AvailabilityKnown: true}
	k := NewKeys(nil, f)
	k.Now = func() time.Time { return time.Now().UTC() }
	got, err := k.VisibleModels(t.Context(), []cpamp.Model{{ID: "shared"}})
	want := []cpamp.Model{{ID: "shared"}, {ID: "anti-only", Object: "model", OwnedBy: "antigravity"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered alias leaked: %+v err=%v", got, err)
	}
}
