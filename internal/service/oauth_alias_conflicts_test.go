package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

func TestCrossChannelAliasSave(t *testing.T) {
	for _, tc := range []struct {
		name     string
		aliases  []cpamp.OAuthModelAlias
		excluded []string
		input    string
		keep     bool
		conflict bool
	}{
		{name: "alias collision ignores case", aliases: []cpamp.OAuthModelAlias{{Name: "shared", Alias: "DAILY"}}, input: "daily", conflict: true},
		{name: "alias collides with original", input: "shared", conflict: true},
		{name: "original collides with other alias", aliases: []cpamp.OAuthModelAlias{{Name: "shared", Alias: "anti-only"}}, input: "unique", keep: true, conflict: true},
		{name: "hidden original is available", aliases: []cpamp.OAuthModelAlias{{Name: "shared", Alias: "codex-only"}}, input: "shared"},
		{name: "fork reclaims original", aliases: []cpamp.OAuthModelAlias{{Name: "shared", Alias: "codex-only", Fork: true}}, input: "shared", conflict: true},
		{name: "disabled aliases are available", aliases: []cpamp.OAuthModelAlias{{Name: "shared", Alias: "daily", Fork: true}}, excluded: []string{"shared"}, input: "daily"},
		{name: "wildcard disabled original is available", excluded: []string{"sha*"}, input: "shared"},
		{name: "shared originals allowed", input: "unique", keep: true},
		{name: "same alias same real model still conflicts", aliases: []cpamp.OAuthModelAlias{{Name: "shared", Alias: "daily"}}, input: "daily", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChannelCPAMP()
			f.aliases["codex"] = tc.aliases
			f.excluded["codex"] = tc.excluded
			k := NewKeys(nil, f)
			_, revision, err := k.OAuthModelAliases(t.Context(), "antigravity")
			if err != nil {
				t.Fatal(err)
			}
			before := append([]cpamp.OAuthModelAlias(nil), f.aliases["antigravity"]...)
			inputs := []OAuthModelAliasInput{
				{Model: "shared", Aliases: "anti-shared"},
				{Model: "anti-only", Aliases: tc.input, KeepOriginal: tc.keep},
			}
			if tc.name == "shared originals allowed" {
				inputs[0] = OAuthModelAliasInput{Model: "shared", KeepOriginal: true}
			}
			if tc.name == "same alias same real model still conflicts" {
				inputs[0] = OAuthModelAliasInput{Model: "shared", Aliases: tc.input}
				inputs[1] = OAuthModelAliasInput{Model: "anti-only", KeepOriginal: true}
			}
			err = k.SetOAuthModelAliases(t.Context(), inputs, revision, "antigravity")
			if tc.conflict {
				if err == nil || !strings.Contains(err.Error(), "跨渠道冲突") || !strings.Contains(err.Error(), "Codex") || !strings.Contains(err.Error(), "Antigravity") {
					t.Fatalf("conflict error = %v", err)
				}
				if len(f.writes) != 0 || !reflect.DeepEqual(before, f.aliases["antigravity"]) {
					t.Fatalf("rejected save wrote settings: %v", f.writes)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCrossChannelAliasEnableAndDisable(t *testing.T) {
	f := newChannelCPAMP()
	f.aliases["codex"] = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "anti-only", Fork: true}}
	f.excluded["antigravity"] = []string{"anti-only"}
	k := NewKeys(nil, f)
	if err := k.SetOAuthModelEnabled(t.Context(), "anti-only", true, "antigravity"); err == nil || !strings.Contains(err.Error(), "跨渠道冲突") {
		t.Fatalf("enable error = %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("rejected enable wrote settings")
	}
	// Disabling a conflicting model must still be allowed to resolve conflicts.
	f.excluded["antigravity"] = nil
	if err := k.SetOAuthModelEnabled(t.Context(), "anti-only", false, "antigravity"); err != nil {
		t.Fatal(err)
	}
}

func TestCrossChannelPresetRejectsBeforeWrites(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	snapshot, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Channels[1].Aliases = append(snapshot.Channels[1].Aliases, cpamp.OAuthModelAlias{Name: "anti-only", Alias: "CODEX-PUBLIC"})
	if err := k.ApplyOAuthPreset(t.Context(), snapshot); err == nil || !strings.Contains(err.Error(), "跨渠道冲突") {
		t.Fatalf("preset error = %v", err)
	}
	if len(f.writes) != 0 || f.configWrites != 0 {
		t.Fatal("rejected preset wrote settings")
	}
}

func TestCrossChannelPresetChecksFinalNotIntermediateState(t *testing.T) {
	f := newChannelCPAMP()
	f.aliases["codex"] = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "anti-only", Fork: true}}
	f.aliases["antigravity"] = append(f.aliases["antigravity"], cpamp.OAuthModelAlias{Name: "anti-only", Alias: "anti-unique"})
	k := NewKeys(nil, f)
	snapshot, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ApplyOAuthPreset(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	after, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil || !OAuthPresetSnapshotsEqual(snapshot, after) {
		t.Fatalf("preset = %+v, err = %v", after, err)
	}
}

func TestCrossChannelRestoringOriginalAndRemovingConflict(t *testing.T) {
	f := newChannelCPAMP()
	f.aliases["codex"] = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "anti-only", Fork: true}}
	f.aliases["antigravity"] = append(f.aliases["antigravity"], cpamp.OAuthModelAlias{Name: "anti-only", Alias: "safe-name"})
	k := NewKeys(nil, f)
	_, revision, err := k.OAuthModelAliases(t.Context(), "antigravity")
	if err != nil {
		t.Fatal(err)
	}
	inputs := []OAuthModelAliasInput{{Model: "shared", Aliases: "anti-public", KeepOriginal: true}, {Model: "anti-only", Aliases: "safe-name", KeepOriginal: true}}
	if err := k.SetOAuthModelAliases(t.Context(), inputs, revision, "antigravity"); err == nil || !strings.Contains(err.Error(), "跨渠道冲突") {
		t.Fatalf("restore original error = %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("rejected original restore wrote settings")
	}
	inputs[1].KeepOriginal = false
	if err := k.SetOAuthModelAliases(t.Context(), inputs, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
	// Existing conflicts can be fixed by replacing the offending alias.
	f.aliases["codex"] = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "safe-name", Fork: true}}
	_, revision, err = k.OAuthModelAliases(t.Context(), "antigravity")
	if err != nil {
		t.Fatal(err)
	}
	inputs[1].Aliases = "renamed-safe"
	if err := k.SetOAuthModelAliases(t.Context(), inputs, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
}

type conflictDiscoveryCPAMP struct {
	*channelCPAMP
	discovered   []string
	discoveryErr error
	fail         string
}

func (f *conflictDiscoveryCPAMP) ListOAuthChannels(context.Context) ([]string, error) {
	return f.discovered, f.discoveryErr
}
func (f *conflictDiscoveryCPAMP) ListAuthFiles(ctx context.Context) ([]cpamp.AuthFile, error) {
	if f.fail == "auth-files" {
		return nil, errors.New("unavailable")
	}
	return f.channelCPAMP.ListAuthFiles(ctx)
}
func (f *conflictDiscoveryCPAMP) ListOAuthModelDefinitions(ctx context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	if channel == "claude" && f.fail == "models" {
		return nil, errors.New("unavailable")
	}
	return f.channelCPAMP.ListOAuthModelDefinitions(ctx, channel)
}
func (f *conflictDiscoveryCPAMP) ListOAuthExcludedModels(ctx context.Context, channel string) ([]string, error) {
	if channel == "claude" && f.fail == "excluded" {
		return nil, errors.New("unavailable")
	}
	return f.channelCPAMP.ListOAuthExcludedModels(ctx, channel)
}
func (f *conflictDiscoveryCPAMP) ListOAuthModelAliases(ctx context.Context, channel string) ([]cpamp.OAuthModelAlias, error) {
	if channel == "claude" && f.fail == "aliases" {
		return nil, errors.New("unavailable")
	}
	return f.channelCPAMP.ListOAuthModelAliases(ctx, channel)
}

func TestCrossChannelConfiguredOnlyAndFailClosed(t *testing.T) {
	for _, failure := range []string{"", "auth-files", "discovery", "models", "excluded", "aliases"} {
		t.Run(failure, func(t *testing.T) {
			f := &conflictDiscoveryCPAMP{channelCPAMP: newChannelCPAMP(), discovered: []string{"claude"}, fail: failure}
			f.models["claude"] = []cpamp.OAuthModelDefinition{{ID: "claude-model"}}
			f.aliases["claude"] = []cpamp.OAuthModelAlias{{Name: "claude-model", Alias: "daily"}}
			if failure == "discovery" {
				f.discoveryErr = errors.New("unavailable")
			}
			k := NewKeys(nil, f)
			_, revision, err := k.OAuthModelAliases(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = k.SetOAuthModelAliases(t.Context(), []OAuthModelAliasInput{{Model: "shared", Aliases: "daily", KeepOriginal: true}}, revision)
			if err == nil {
				t.Fatal("save bypassed unavailable/conflicting channel")
			}
			if failure == "" && !strings.Contains(err.Error(), "Claude") {
				t.Fatalf("configured-only conflict = %v", err)
			}
			if failure != "" && !strings.Contains(err.Error(), "无法完成跨渠道别名检查") {
				t.Fatalf("failure = %v", err)
			}
			if len(f.writes) != 0 {
				t.Fatal("failed check wrote settings")
			}
		})
	}
}

func TestConcurrentCrossChannelAliasSavesCannotBothClaimName(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	revisions := map[string]string{}
	for _, channel := range []string{"codex", "antigravity"} {
		_, revision, err := k.OAuthModelAliases(t.Context(), channel)
		if err != nil {
			t.Fatal(err)
		}
		revisions[channel] = revision
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, channel := range []string{"codex", "antigravity"} {
		wg.Add(1)
		go func(channel string) {
			defer wg.Done()
			<-start
			inputs := []OAuthModelAliasInput{{Model: "shared", Aliases: "daily", KeepOriginal: true}}
			if channel == "antigravity" {
				inputs = append(inputs, OAuthModelAliasInput{Model: "anti-only", KeepOriginal: true})
			}
			results <- k.SetOAuthModelAliases(t.Context(), inputs, revisions[channel], channel)
		}(channel)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "跨渠道冲突") {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}
