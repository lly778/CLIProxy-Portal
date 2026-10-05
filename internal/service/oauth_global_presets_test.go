package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

func TestGlobalPresetMovesAliasBetweenChannels(t *testing.T) {
	f := newChannelCPAMP()
	f.aliases["codex"] = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "daily", Fork: true}}
	k := NewKeys(nil, f)
	target, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target.Channels[0].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "codex-only", Fork: true}}
	target.Channels[1].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "daily", Fork: true}}
	if err := k.ApplyOAuthPreset(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	actual, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil || !OAuthPresetSnapshotsEqual(target, actual) {
		t.Fatalf("applied=%+v err=%v", actual, err)
	}
}

func TestGlobalPresetPreservesUnrecordedChannelAndChecksConflicts(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	current, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	partial := OAuthPresetSnapshot{Version: OAuthPresetVersion, Channels: []OAuthChannelPreset{current.Channels[1]}}
	partial.Channels[0].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "new-anti", Fork: true}}
	if err := k.ApplyOAuthPreset(t.Context(), partial); err != nil {
		t.Fatal(err)
	}
	for _, write := range f.writes {
		if strings.HasPrefix(write, "codex:") {
			t.Fatal("unrecorded channel rewritten")
		}
	}
	partial.Channels[0].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "codex-public", Fork: true}}
	count := len(f.writes)
	if err := k.ApplyOAuthPreset(t.Context(), partial); err == nil || !strings.Contains(err.Error(), "跨渠道冲突") {
		t.Fatalf("error=%v", err)
	}
	if len(f.writes) != count {
		t.Fatal("conflicting partial preset wrote settings")
	}
}

type failingGlobalPresetAPI struct {
	*channelCPAMP
	failed bool
	cancel context.CancelFunc
}

func (f *failingGlobalPresetAPI) SetOAuthModelAliases(ctx context.Context, channel string, aliases []cpamp.OAuthModelAlias) error {
	if channel == "antigravity" && !f.failed {
		f.failed = true
		if f.cancel != nil {
			f.cancel()
		}
		return errors.New("injected second-channel write failure")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.channelCPAMP.SetOAuthModelAliases(ctx, channel, aliases)
}

func TestGlobalPresetRollsBackEveryTouchedChannelEvenAfterCancel(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancellation"}[cancelRequest], func(t *testing.T) {
			f := &failingGlobalPresetAPI{channelCPAMP: newChannelCPAMP()}
			k := NewKeys(nil, f)
			before, err := k.OAuthPresetSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			target, err := k.OAuthPresetSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			target.Channels[0].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "changed-codex", Fork: true}}
			target.Channels[1].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "changed-anti", Fork: true}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelRequest {
				f.cancel = cancel
			}
			if err := k.ApplyOAuthPreset(ctx, target); err == nil || !strings.Contains(err.Error(), "已恢复所有受影响渠道") {
				t.Fatalf("apply error=%v", err)
			}
			after, err := k.OAuthPresetSnapshot(t.Context())
			if err != nil || !OAuthPresetSnapshotsEqual(before, after) {
				t.Fatalf("rollback=%+v err=%v", after, err)
			}
		})
	}
}

func TestGlobalPresetAllChannelsValidatedBeforeAnyWrite(t *testing.T) {
	for _, failure := range []string{"catalog", "alias", "duplicate-channel", "missing-channel", "unknown-alias"} {
		t.Run(failure, func(t *testing.T) {
			f := newChannelCPAMP()
			k := NewKeys(nil, f)
			target, err := k.OAuthPresetSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "catalog":
				target.Channels[1].Models = target.Channels[1].Models[:1]
			case "alias":
				target.Channels[1].Aliases = []cpamp.OAuthModelAlias{{Name: "shared", Alias: "not valid"}}
			case "duplicate-channel":
				target.Channels = append(target.Channels, target.Channels[0])
			case "missing-channel":
				target.Channels[1].Channel = "removed-provider"
			case "unknown-alias":
				f.aliases["codex"] = append(f.aliases["codex"], cpamp.OAuthModelAlias{Name: "plugin-not-in-catalog", Alias: "anti-public"})
			}
			if err := k.ApplyOAuthPreset(t.Context(), target); err == nil {
				t.Fatal("invalid target accepted")
			}
			if len(f.writes) != 0 || f.configWrites != 0 {
				t.Fatal("invalid final target mutated CPA")
			}
		})
	}
}

func TestGlobalPresetEqualityIgnoresChannelOrderButNotChannelChanges(t *testing.T) {
	f := newChannelCPAMP()
	k := NewKeys(nil, f)
	left, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	right, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	right.Channels[0], right.Channels[1] = right.Channels[1], right.Channels[0]
	if !OAuthPresetSnapshotsEqual(left, right) {
		t.Fatal("channel order changed equality")
	}
	right.Channels[0].Channel = "other"
	if OAuthPresetSnapshotsEqual(left, right) {
		t.Fatal("different channel matched")
	}
}
