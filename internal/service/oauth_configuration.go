package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"cliproxy-portal/internal/cpamp"
)

var ErrOAuthConfigurationChanged = errors.New("上游配置已变化，请重新打开配置弹窗")

// OAuthConfigurationRevision covers all edited channels, including their catalogs.
func OAuthConfigurationRevision(snapshot OAuthPresetSnapshot) string {
	snapshot.Channels = append([]OAuthChannelPreset(nil), snapshot.Channels...)
	for index := range snapshot.Channels {
		entry := &snapshot.Channels[index]
		entry.Models = append([]OAuthPresetModel(nil), entry.Models...)
		entry.Aliases = append(entry.Aliases[:0:0], entry.Aliases...)
		canonicalizeOAuthPreset(entry)
	}
	sort.Slice(snapshot.Channels, func(i, j int) bool { return snapshot.Channels[i].Channel < snapshot.Channels[j].Channel })
	data, _ := json.Marshal(snapshot)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (k *Keys) OAuthChannelConfiguration(ctx context.Context, channel string) (OAuthPresetSnapshot, error) {
	return upstreamView(ctx, k, "snapshot:"+channel, func(ctx context.Context) (OAuthPresetSnapshot, error) {
		k.presetMu.Lock()
		defer k.presetMu.Unlock()
		entry, err := k.oauthChannelPresetSnapshot(ctx, channel)
		if err != nil {
			return OAuthPresetSnapshot{}, err
		}
		return OAuthPresetSnapshot{Version: OAuthPresetVersion, Channels: []OAuthChannelPreset{entry}}, nil
	}, cloneOAuthSnapshot)
}

// ApplyOAuthConfiguration shares the preset transaction without creating a preset.
func (k *Keys) ApplyOAuthConfiguration(ctx context.Context, requested OAuthPresetSnapshot, revision string) error {
	ctx = cpamp.WithFreshUpstreamReads(ctx)
	k.InvalidateUpstreamReadCache()
	defer k.InvalidateUpstreamReadCache()
	k.presetMu.Lock()
	defer k.presetMu.Unlock()
	if len(requested.Channels) == 0 || revision == "" {
		return errors.New("上游配置内容无效")
	}
	current, target, included, err := k.prepareOAuthPreset(ctx, requested)
	if err != nil {
		return err
	}
	edited := OAuthPresetSnapshot{Version: OAuthPresetVersion}
	for _, entry := range current.Channels {
		if included[entry.Channel] {
			edited.Channels = append(edited.Channels, entry)
		}
	}
	if OAuthConfigurationRevision(edited) != revision {
		return ErrOAuthConfigurationChanged
	}
	if OAuthPresetSnapshotsEqual(edited, requested) {
		return nil
	}
	return k.applyPreparedOAuthPreset(ctx, current, target, included)
}
