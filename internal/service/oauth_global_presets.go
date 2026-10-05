package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
)

func (k *Keys) OAuthPresetSnapshot(ctx context.Context) (OAuthPresetSnapshot, error) {
	k.presetMu.Lock()
	defer k.presetMu.Unlock()
	return k.oauthPresetSnapshot(ctx)
}

func (k *Keys) oauthPresetSnapshot(ctx context.Context) (OAuthPresetSnapshot, error) {
	channels, err := k.oauthChannels(ctx, true)
	if err != nil {
		return OAuthPresetSnapshot{}, err
	}
	snapshot := OAuthPresetSnapshot{Version: OAuthPresetVersion}
	for _, channel := range channels {
		entry, err := k.oauthChannelPresetSnapshot(ctx, channel)
		if err != nil {
			return OAuthPresetSnapshot{}, fmt.Errorf("读取 %s 调用配置失败：%w", OAuthChannelLabel(channel), err)
		}
		snapshot.Channels = append(snapshot.Channels, entry)
	}
	return snapshot, nil
}

// Format migrations belong to the store, not the runtime. Version 3 is the
// only accepted envelope; each included channel must be explicit and unique.
func ValidateOAuthPresetFormat(snapshot OAuthPresetSnapshot) error {
	if snapshot.Version != OAuthPresetVersion || len(snapshot.Channels) == 0 {
		return errors.New("调用预设内容无效，请重新保存")
	}
	seen := map[string]bool{}
	for _, entry := range snapshot.Channels {
		channel, err := oauthPresetChannel(entry)
		if err != nil {
			return err
		}
		if seen[channel] {
			return errors.New("调用预设包含重复渠道，请重新保存")
		}
		seen[channel] = true
	}
	return nil
}

func OAuthPresetSnapshotsEqual(left, right OAuthPresetSnapshot) bool {
	if ValidateOAuthPresetFormat(left) != nil || ValidateOAuthPresetFormat(right) != nil || len(left.Channels) != len(right.Channels) {
		return false
	}
	byChannel := map[string]OAuthChannelPreset{}
	for _, entry := range right.Channels {
		channel, _ := oauthPresetChannel(entry)
		byChannel[channel] = entry
	}
	for _, entry := range left.Channels {
		channel, _ := oauthPresetChannel(entry)
		other, ok := byChannel[channel]
		if !ok || !oauthChannelPresetsEqual(entry, other) {
			return false
		}
	}
	return true
}

// Compare the combined final target, not one channel's target against another
// channel's old aliases. This allows an alias to move between channels.
func validatePresetChannelNames(snapshot OAuthPresetSnapshot) error {
	type owner struct {
		channel string
		name    oauthVisibleName
	}
	claimed := map[string][]owner{}
	for _, entry := range snapshot.Channels {
		models := make([]OAuthModelSetting, 0, len(entry.Models))
		for _, model := range entry.Models {
			models = append(models, OAuthModelSetting{ID: model.ID, Enabled: model.Enabled})
		}
		for _, name := range oauthVisibleNames(models, entry.Aliases) {
			key := strings.ToLower(name.name)
			for _, previous := range claimed[key] {
				if previous.channel != entry.Channel && (name.alias || previous.name.alias) {
					return fmt.Errorf("用户侧名称 %s 跨渠道冲突：%s 的 %s 与 %s 的 %s", name.name, OAuthChannelLabel(entry.Channel), name.model, OAuthChannelLabel(previous.channel), previous.name.model)
				}
			}
			claimed[key] = append(claimed[key], owner{channel: entry.Channel, name: name})
		}
	}
	return nil
}

func (k *Keys) ApplyOAuthPreset(ctx context.Context, requested OAuthPresetSnapshot) error {
	k.presetMu.Lock()
	defer k.presetMu.Unlock()
	if err := ValidateOAuthPresetFormat(requested); err != nil {
		return err
	}
	current, err := k.oauthPresetSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("读取当前调用配置失败：%w", err)
	}
	target := OAuthPresetSnapshot{Version: OAuthPresetVersion, Channels: append([]OAuthChannelPreset(nil), current.Channels...)}
	indices := map[string]int{}
	included := map[string]bool{}
	for i, entry := range target.Channels {
		indices[entry.Channel] = i
	}
	for _, entry := range requested.Channels {
		channel, _ := oauthPresetChannel(entry)
		index, ok := indices[channel]
		if !ok {
			return fmt.Errorf("预设中的 %s 渠道已不存在，请重新保存", OAuthChannelLabel(channel))
		}
		entry.Channel = channel
		target.Channels[index] = entry
		included[channel] = true
	}
	// Channels not recorded by a preset (e.g. newly installed providers) remain
	// unchanged, but still participate in the final cross-channel collision check.
	for _, entry := range target.Channels {
		if err := k.validateOAuthChannelPreset(ctx, entry); err != nil {
			return fmt.Errorf("%s：%w", OAuthChannelLabel(entry.Channel), err)
		}
	}
	// Unknown/static-catalog-lagging aliases are preserved by the setters and
	// must participate in validation even though presets do not manage them.
	routingTarget := OAuthPresetSnapshot{Version: OAuthPresetVersion, Channels: append([]OAuthChannelPreset(nil), target.Channels...)}
	for i, entry := range routingTarget.Channels {
		known := map[string]bool{}
		for _, model := range entry.Models {
			known[strings.ToLower(model.ID)] = true
		}
		liveAliases, _, err := k.OAuthModelAliases(ctx, entry.Channel)
		if err != nil {
			return err
		}
		routingTarget.Channels[i].Aliases = append([]cpamp.OAuthModelAlias(nil), entry.Aliases...)
		for _, alias := range liveAliases {
			if !known[strings.ToLower(strings.TrimSpace(alias.Name))] {
				routingTarget.Channels[i].Aliases = append(routingTarget.Channels[i].Aliases, alias)
			}
		}
	}
	if err := validatePresetChannelNames(routingTarget); err != nil {
		return err
	}
	attempted := []OAuthChannelPreset{}
	for _, entry := range target.Channels {
		// Do not rewrite channels absent from the stored preset.
		if !included[entry.Channel] {
			continue
		}
		attempted = append(attempted, current.Channels[indices[entry.Channel]])
		if err := k.applyOAuthChannelPreset(ctx, entry); err != nil {
			// Cancellation must not prevent a best-effort rollback across every
			// already-touched channel, including the one that failed halfway through.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
			defer cancel()
			rollbackErrors := []error{}
			for i := len(attempted) - 1; i >= 0; i-- {
				if restoreErr := k.applyOAuthChannelPreset(rollbackCtx, attempted[i]); restoreErr != nil {
					rollbackErrors = append(rollbackErrors, fmt.Errorf("%s：%w", attempted[i].Channel, restoreErr))
				}
			}
			if len(rollbackErrors) > 0 {
				return fmt.Errorf("应用预设失败：%v；恢复原配置也失败：%w", err, errors.Join(rollbackErrors...))
			}
			return fmt.Errorf("应用预设失败，已恢复所有受影响渠道：%w", err)
		}
	}
	return nil
}
