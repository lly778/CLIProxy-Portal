package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

type channelCPAMP struct {
	cpamp.API
	files         []cpamp.AuthFile
	models        map[string][]cpamp.OAuthModelDefinition
	aliases       map[string][]cpamp.OAuthModelAlias
	excluded      map[string][]string
	writes        []string
	failAliasOnce bool
	configCalls   int
	config        []byte
	configWrites  int
}

func newChannelCPAMP() *channelCPAMP {
	return &channelCPAMP{
		config:   []byte("api-keys: []\n"),
		files:    []cpamp.AuthFile{{Name: "codex.json", Provider: "codex"}, {Name: "anti.json", Provider: "antigravity"}},
		models:   map[string][]cpamp.OAuthModelDefinition{"codex": {{ID: "shared"}}, "antigravity": {{ID: "shared"}, {ID: "anti-only"}}},
		aliases:  map[string][]cpamp.OAuthModelAlias{"codex": {{Name: "shared", Alias: "codex-public", Fork: true}}, "antigravity": {{Name: "shared", Alias: "anti-public", Fork: true}}},
		excluded: map[string][]string{"codex": {"codex-hidden"}, "antigravity": {}},
	}
}
func (f *channelCPAMP) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	return append([]cpamp.AuthFile(nil), f.files...), nil
}
func (f *channelCPAMP) SetAuthFileDisabled(_ context.Context, file cpamp.AuthFile, disabled bool) error {
	for i := range f.files {
		if f.files[i].Name == file.Name && f.files[i].Provider == file.Provider {
			f.files[i].Disabled = disabled
			f.writes = append(f.writes, file.Provider+":account")
			return nil
		}
	}
	return errors.New("unknown file")
}
func (f *channelCPAMP) ListOAuthModelDefinitions(_ context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	return f.models[channel], nil
}
func (f *channelCPAMP) ListOAuthExcludedModels(_ context.Context, channel string) ([]string, error) {
	return append([]string(nil), f.excluded[channel]...), nil
}
func (f *channelCPAMP) SetOAuthExcludedModels(_ context.Context, channel string, models []string) error {
	f.excluded[channel] = append([]string(nil), models...)
	f.writes = append(f.writes, channel+":excluded")
	return nil
}
func (f *channelCPAMP) ListOAuthModelAliases(_ context.Context, channel string) ([]cpamp.OAuthModelAlias, error) {
	return append([]cpamp.OAuthModelAlias(nil), f.aliases[channel]...), nil
}
func (f *channelCPAMP) SetOAuthModelAliases(_ context.Context, channel string, aliases []cpamp.OAuthModelAlias) error {
	f.writes = append(f.writes, channel+":aliases")
	if f.failAliasOnce {
		f.failAliasOnce = false
		return errors.New("temporary alias failure")
	}
	f.aliases[channel] = append([]cpamp.OAuthModelAlias(nil), aliases...)
	return nil
}
func (f *channelCPAMP) GetConfigYAML(context.Context) ([]byte, error) {
	f.configCalls++
	return append([]byte(nil), f.config...), nil
}
func (f *channelCPAMP) PutConfigYAML(_ context.Context, data []byte) error {
	f.configCalls++
	f.configWrites++
	f.config = append([]byte(nil), data...)
	return nil
}

func TestOAuthChannelDiscoveryAndAccountIsolation(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	channels, err := k.OAuthChannels(t.Context())
	if err != nil || !reflect.DeepEqual(channels, []string{"codex", "antigravity"}) {
		t.Fatalf("channels=%v err=%v", channels, err)
	}
	accounts, err := k.UpstreamAccounts(t.Context(), " ANTIGRAVITY ")
	if err != nil || len(accounts) != 1 || accounts[0].Name != "anti.json" || accounts[0].Provider != "Antigravity" {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	if _, err := k.SetUpstreamAccountDisabled(t.Context(), accounts[0].ID, true); err == nil {
		t.Fatal("cross-channel account accepted")
	}
	if len(f.writes) != 0 {
		t.Fatal("rejected account mutated CPA")
	}
	if _, err := k.SetUpstreamAccountDisabled(t.Context(), accounts[0].ID, true, "antigravity"); err != nil {
		t.Fatal(err)
	}
	if f.files[0].Disabled || !f.files[1].Disabled {
		t.Fatalf("files=%+v", f.files)
	}
	for _, value := range []string{"../codex", "codex?other=1", "a/b", "*"} {
		if _, err := k.UpstreamAccounts(t.Context(), value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

type unavailableChannelConfig struct{ *channelCPAMP }

func (*unavailableChannelConfig) ListOAuthChannels(context.Context) ([]string, error) {
	return nil, errors.New("configuration endpoint unavailable")
}

func TestOAuthChannelDiscoveryKeepsCredentialsWhenConfigIsUnavailable(t *testing.T) {
	k := NewKeys(nil, &unavailableChannelConfig{newChannelCPAMP()})
	channels, err := k.OAuthChannels(t.Context())
	if err != nil || !reflect.DeepEqual(channels, []string{"codex", "antigravity"}) {
		t.Fatalf("channels=%v err=%v", channels, err)
	}
}

func TestOAuthChannelAliasesAndRevisionsAreIsolated(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	codexBefore := append([]cpamp.OAuthModelAlias(nil), f.aliases["codex"]...)
	_, codexRevision, err := k.OAuthModelAliases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := k.OAuthModelAliases(t.Context(), "antigravity")
	if err != nil {
		t.Fatal(err)
	}
	inputs := []OAuthModelAliasInput{{Model: "shared", Aliases: "renamed", KeepOriginal: true}, {Model: "anti-only", KeepOriginal: true}}
	if err := k.SetOAuthModelAliases(t.Context(), inputs, codexRevision, "antigravity"); err == nil {
		t.Fatal("foreign revision accepted")
	}
	if len(f.writes) != 0 {
		t.Fatal("foreign revision wrote config")
	}
	if err := k.SetOAuthModelAliases(t.Context(), inputs, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(codexBefore, f.aliases["codex"]) || f.aliases["antigravity"][0].Alias != "renamed" {
		t.Fatalf("aliases=%v", f.aliases)
	}
	if err := k.SetOAuthModelAliases(t.Context(), inputs, revision, "antigravity"); err == nil {
		t.Fatal("stale revision accepted")
	}
	if oauthChannelAliasRevision("codex", nil) == oauthChannelAliasRevision("antigravity", nil) {
		t.Fatal("identical empty configs share revision")
	}
}

func TestOAuthChannelDisableAndRollbackNeverTouchCodex(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "disable", true: "rollback"}[fail], func(t *testing.T) {
			f := newChannelCPAMP()
			f.failAliasOnce = fail
			k := NewKeys(nil, f)
			err := k.SetOAuthModelEnabled(t.Context(), "shared", false, "antigravity")
			if (err != nil) != fail {
				t.Fatalf("err=%v", err)
			}
			if f.configWrites != 0 || len(f.aliases["codex"]) != 1 || !reflect.DeepEqual(f.excluded["codex"], []string{"codex-hidden"}) {
				t.Fatal("Codex settings touched")
			}
			rule, _ := excludedModelRule("shared", f.excluded["antigravity"])
			if fail && rule != "" {
				t.Fatal("failed mutation did not roll back")
			}
			if !fail && (rule == "" || len(f.aliases["antigravity"]) != 0) {
				t.Fatal("model state not changed")
			}
			for _, write := range f.writes {
				if !strings.HasPrefix(write, "antigravity:") {
					t.Fatalf("foreign write: %s", write)
				}
			}
		})
	}
}

func TestOAuthChannelPresetSnapshotAndApplyAreIsolated(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	snapshot, err := k.OAuthPresetSnapshot(t.Context(), "antigravity")
	if err != nil || snapshot.Channel != "antigravity" || len(snapshot.ReasoningCaps) != 0 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if err := k.SetOAuthModelEnabled(t.Context(), "shared", false, "antigravity"); err != nil {
		t.Fatal(err)
	}
	if err := k.ApplyOAuthPreset(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := k.OAuthPresetSnapshot(t.Context(), "antigravity")
	if err != nil || !OAuthPresetSnapshotsEqual(snapshot, restored) {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	if f.configWrites != 0 || len(f.aliases["codex"]) != 1 || !reflect.DeepEqual(f.excluded["codex"], []string{"codex-hidden"}) {
		t.Fatal("Codex config changed")
	}
	foreign := snapshot
	foreign.Channel = "codex"
	if OAuthPresetSnapshotsEqual(snapshot, foreign) {
		t.Fatal("presets from different channels matched")
	}
	legacy := OAuthPresetSnapshot{Version: 1}
	modern := OAuthPresetSnapshot{Version: OAuthPresetVersion, Channel: "codex"}
	if OAuthPresetSnapshotsEqual(legacy, modern) {
		t.Fatal("legacy preset treated as current format")
	}
	if err := k.ApplyOAuthPreset(t.Context(), legacy); err == nil {
		t.Fatal("legacy preset accepted")
	}
	missingChannel := modern
	missingChannel.Channel = ""
	if err := k.ApplyOAuthPreset(t.Context(), missingChannel); err == nil {
		t.Fatal("missing channel accepted as Codex")
	}
	snapshot.ReasoningCaps = map[string]string{"shared": "high"}
	writes := len(f.writes)
	if err := k.ApplyOAuthPreset(t.Context(), snapshot); err == nil || len(f.writes) != writes {
		t.Fatal("unsupported reasoning preset was applied")
	}
}
