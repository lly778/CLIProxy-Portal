package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
)

type oauthVisibleName struct {
	name  string
	model string
	alias bool
}

// Disabled models claim neither aliases nor originals. Renamed models claim
// their original only when fork is enabled. Preserve unknown configured models
// conservatively, as the local static catalog may lag CPA/plugin definitions.
func oauthVisibleNames(models []OAuthModelSetting, aliases []cpamp.OAuthModelAlias) []oauthVisibleName {
	known := make(map[string]bool, len(models))
	grouped := make(map[string][]cpamp.OAuthModelAlias)
	for _, entry := range aliases {
		key := strings.ToLower(strings.TrimSpace(entry.Name))
		grouped[key] = append(grouped[key], entry)
	}
	names := []oauthVisibleName{}
	appendAliases := func(model string, entries []cpamp.OAuthModelAlias) bool {
		keepOriginal := true
		for _, entry := range entries {
			alias := strings.TrimSpace(entry.Alias)
			if alias != "" && !strings.EqualFold(alias, model) {
				names = append(names, oauthVisibleName{name: alias, model: model, alias: true})
				keepOriginal = false
			}
		}
		for _, entry := range entries {
			keepOriginal = keepOriginal || entry.Fork
		}
		return keepOriginal
	}
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		known[strings.ToLower(id)] = true
		if model.Enabled && appendAliases(id, grouped[strings.ToLower(id)]) {
			names = append(names, oauthVisibleName{name: id, model: id})
		}
	}
	for id, entries := range grouped {
		if known[id] {
			continue
		}
		for _, entry := range entries {
			model, alias := strings.TrimSpace(entry.Name), strings.TrimSpace(entry.Alias)
			if alias != "" {
				names = append(names, oauthVisibleName{name: alias, model: model, alias: !strings.EqualFold(alias, model)})
			}
			if entry.Fork {
				names = append(names, oauthVisibleName{name: model, model: model})
			}
		}
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := names[i], names[j]
		if strings.ToLower(left.name) != strings.ToLower(right.name) {
			return strings.ToLower(left.name) < strings.ToLower(right.name)
		}
		return left.model < right.model
	})
	return names
}

// Caller serializes portal routing writes with presetMu. Only conflicts
// involving an alias are forbidden: shared real model IDs are intentional CPA
// routing pools. Never compare unrelated pairs of other channels.
func (k *Keys) validateCrossChannelAliases(ctx context.Context, channel string, models []OAuthModelSetting, aliases []cpamp.OAuthModelAlias) error {
	channels, err := k.oauthChannels(ctx, true)
	if err != nil {
		return fmt.Errorf("无法完成跨渠道别名检查：读取渠道失败：%w", err)
	}
	current := oauthVisibleNames(models, aliases)
	for _, other := range channels {
		if other == channel {
			continue
		}
		otherModels, _, err := k.OAuthModelSettings(ctx, other)
		if err != nil {
			return fmt.Errorf("无法完成跨渠道别名检查：读取 %s 模型失败：%w", OAuthChannelLabel(other), err)
		}
		otherAliases, _, err := k.OAuthModelAliases(ctx, other)
		if err != nil {
			return fmt.Errorf("无法完成跨渠道别名检查：读取 %s 别名失败：%w", OAuthChannelLabel(other), err)
		}
		occupied := make(map[string][]oauthVisibleName)
		for _, name := range oauthVisibleNames(otherModels, otherAliases) {
			key := strings.ToLower(name.name)
			occupied[key] = append(occupied[key], name)
		}
		for _, name := range current {
			for _, existing := range occupied[strings.ToLower(name.name)] {
				if name.alias || existing.alias {
					return fmt.Errorf("用户侧名称 %s 跨渠道冲突：%s 的 %s 与 %s 的 %s，请调整别名或原名设置", name.name, OAuthChannelLabel(channel), name.model, OAuthChannelLabel(other), existing.model)
				}
			}
		}
	}
	return nil
}
