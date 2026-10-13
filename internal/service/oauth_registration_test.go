package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

// Configuration writes deliberately leave the registry stale. Only the status
// hook refreshes it, as in the production failure this change addresses.
type registrationCPAMP struct {
	*channelCPAMP
	registered map[string][]cpamp.Model
	refreshes  []string
	staleFor   int
	failOnce   string
	failAlways bool
	onRefresh  func()
}

func newRegistrationCPAMP(t *testing.T) *registrationCPAMP {
	t.Helper()
	f := &registrationCPAMP{channelCPAMP: newChannelCPAMP(), registered: map[string][]cpamp.Model{}}
	f.files = append(f.files, cpamp.AuthFile{Name: "codex-second.json", Provider: "codex"}, cpamp.AuthFile{Name: "codex-disabled.json", Provider: "codex", Disabled: true})
	for _, file := range f.files {
		models, err := f.channelCPAMP.ListAuthFileModels(t.Context(), file)
		if err != nil {
			t.Fatal(err)
		}
		f.registered[file.Name] = models
	}
	return f
}

func (f *registrationCPAMP) ListAuthFileModels(_ context.Context, file cpamp.AuthFile) ([]cpamp.Model, error) {
	return append([]cpamp.Model(nil), f.registered[file.Name]...), nil
}

func (f *registrationCPAMP) SetAuthFileDisabled(ctx context.Context, file cpamp.AuthFile, disabled bool) error {
	if file.Disabled || disabled {
		return errors.New("registration refresh must not toggle disabled credentials")
	}
	f.refreshes = append(f.refreshes, file.Name)
	if f.onRefresh != nil {
		f.onRefresh()
	}
	if f.failAlways || f.failOnce == file.Name {
		f.failOnce = ""
		return errors.New("injected registration failure")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.staleFor > 0 {
		f.staleFor--
		return nil
	}
	models, err := f.channelCPAMP.ListAuthFileModels(ctx, file)
	f.registered[file.Name] = models
	return err
}

func TestOAuthSettingRefreshesAllEnabledAccountsOnlyInSelectedChannel(t *testing.T) {
	for _, operation := range []string{"aliases", "model", "reasoning", "unchanged-aliases"} {
		t.Run(operation, func(t *testing.T) {
			f := newRegistrationCPAMP(t)
			k := NewKeys(nil, f)
			var err error
			switch operation {
			case "aliases", "unchanged-aliases":
				_, revision, _ := k.OAuthModelAliases(t.Context(), "codex")
				alias := "new-public"
				if operation == "unchanged-aliases" {
					alias = "codex-public"
				}
				err = k.SetOAuthModelAliases(t.Context(), []OAuthModelAliasInput{{Model: "shared", Aliases: alias, KeepOriginal: true}}, revision, "codex")
			case "model":
				err = k.SetOAuthModelEnabled(t.Context(), "shared", false, "codex")
			case "reasoning":
				f.models["codex"][0].ThinkingLevels = []string{"low", "medium", "high"}
				_, revision, _ := k.OAuthReasoningCaps(t.Context(), "codex")
				err = k.SetOAuthReasoningCaps(t.Context(), []ReasoningCapInput{{Model: "shared", Cap: "medium"}}, revision, "codex")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.refreshes, []string{"codex.json", "codex-second.json"}) || !f.files[3].Disabled {
				t.Fatalf("refreshes=%v files=%v", f.refreshes, f.files)
			}
			for _, file := range f.files[:3] {
				if file.Provider != "codex" {
					continue
				}
				want, _ := f.channelCPAMP.ListAuthFileModels(t.Context(), file)
				if !reflect.DeepEqual(f.registered[file.Name], want) {
					t.Fatalf("stale registry for %s: %v want %v", file.Name, f.registered[file.Name], want)
				}
			}
		})
	}
}

func TestPresetRefreshesAfterAllChannelWritesAndAlsoWhenUnchanged(t *testing.T) {
	f := newRegistrationCPAMP(t)
	k := NewKeys(nil, f)
	target, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target.Channels[0].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "new-codex", Fork: true}}
	target.Channels[1].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "new-anti", Fork: true}}
	f.onRefresh = func() {
		for _, entry := range target.Channels {
			if len(f.aliases[entry.Channel]) != 1 || f.aliases[entry.Channel][0].Alias != entry.Aliases[0].Alias {
				t.Fatal("registration refreshed before final channel settings were written")
			}
		}
	}
	for i := 0; i < 2; i++ {
		f.refreshes = nil
		if err := k.ApplyOAuthPreset(t.Context(), target); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.refreshes, []string{"codex.json", "anti.json", "codex-second.json"}) {
			t.Fatalf("refreshes=%v", f.refreshes)
		}
	}
}

func TestRegistrationRetriesStaleAsyncReload(t *testing.T) {
	f := newRegistrationCPAMP(t)
	f.staleFor = 1
	k := NewKeys(nil, f)
	_, revision, _ := k.OAuthModelAliases(t.Context(), "codex")
	if err := k.SetOAuthModelAliases(t.Context(), []OAuthModelAliasInput{{Model: "shared", Aliases: "new-public", KeepOriginal: true}}, revision); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.refreshes, []string{"codex.json", "codex.json", "codex-second.json"}) {
		t.Fatalf("refreshes=%v", f.refreshes)
	}
}

func TestPresetRegistrationFailureRollsBackSettingsAndRegistry(t *testing.T) {
	f := newRegistrationCPAMP(t)
	k := NewKeys(nil, f)
	before, _ := k.OAuthPresetSnapshot(t.Context())
	target, _ := k.OAuthPresetSnapshot(t.Context())
	target.Channels[0].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "new-codex", Fork: true}}
	target.Channels[1].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "new-anti", Fork: true}}
	f.failOnce = "codex-second.json"
	if err := k.ApplyOAuthPreset(t.Context(), target); err == nil || !strings.Contains(err.Error(), "已恢复所有受影响渠道") {
		t.Fatalf("apply error=%v", err)
	}
	after, _ := k.OAuthPresetSnapshot(t.Context())
	if !OAuthPresetSnapshotsEqual(before, after) {
		t.Fatalf("configuration not restored: %+v", after)
	}
	for _, file := range f.files[:3] {
		want, _ := f.channelCPAMP.ListAuthFileModels(t.Context(), file)
		if !reflect.DeepEqual(want, f.registered[file.Name]) {
			t.Fatalf("registry not restored: %v want %v", f.registered[file.Name], want)
		}
	}
	f.failAlways = true
	if err := k.ApplyOAuthPreset(t.Context(), target); err == nil || !strings.Contains(err.Error(), "恢复 CPA 注册失败") {
		t.Fatalf("rollback registration failure hidden: %v", err)
	}
}

func TestSingleSettingReportsSavedConfigWhenRegistrationFails(t *testing.T) {
	f := newRegistrationCPAMP(t)
	f.failAlways = true
	k := NewKeys(nil, f)
	_, revision, _ := k.OAuthModelAliases(t.Context(), "codex")
	err := k.SetOAuthModelAliases(t.Context(), []OAuthModelAliasInput{{Model: "shared", Aliases: "new-public", KeepOriginal: true}}, revision)
	if err == nil || !strings.Contains(err.Error(), "设置已保存，但 CPA 注册刷新失败") || f.aliases["codex"][0].Alias != "new-public" {
		t.Fatalf("error=%v aliases=%v", err, f.aliases)
	}
}

func TestVerifyRegistrationDoesNotGuessAccountPlanModels(t *testing.T) {
	snapshot := OAuthChannelPreset{Models: []OAuthPresetModel{{ID: "supported", Enabled: true}, {ID: "other-plan", Enabled: true}, {ID: "old", Enabled: false}}, Aliases: []cpamp.OAuthModelAlias{{Name: "supported", Alias: "old", Fork: false}, {Name: "supported", Alias: "new", Fork: false}, {Name: "other-plan", Alias: "not-on-this-plan"}}}
	before := []cpamp.Model{{ID: "team/supported"}}
	after := []cpamp.Model{{ID: "team/old"}, {ID: "team/new"}}
	if err := verifyOAuthRegistration(snapshot, before, after); err != nil {
		t.Fatal(err)
	}
	if err := verifyOAuthRegistration(snapshot, before, after[:1]); err == nil || !strings.Contains(err.Error(), "new") {
		t.Fatalf("missing supported alias was not detected: %v", err)
	}
	if err := verifyOAuthRegistration(snapshot, before, append(after, before...)); err == nil || !strings.Contains(err.Error(), "旧路由") {
		t.Fatalf("hidden original model still registered: %v", err)
	}
}
