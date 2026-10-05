package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
)

const OAuthPresetVersion = 3

// OAuthPresetSnapshot stores portal-managed routing across OAuth channels,
// deliberately excluding credentials and all non-portal CPA configuration.
type OAuthPresetSnapshot struct {
	Version  int                  `json:"version"`
	Channels []OAuthChannelPreset `json:"channels"`
}

type OAuthChannelPreset struct {
	Channel       string                  `json:"channel"`
	Models        []OAuthPresetModel      `json:"models"`
	Aliases       []cpamp.OAuthModelAlias `json:"aliases,omitempty"`
	ReasoningCaps map[string]string       `json:"reasoning_caps,omitempty"`
}

type OAuthPresetModel struct {
	ID           string `json:"id"`
	Enabled      bool   `json:"enabled"`
	WildcardRule string `json:"wildcard_rule,omitempty"`
}

func oauthPresetChannel(snapshot OAuthChannelPreset) (string, error) {
	if strings.TrimSpace(snapshot.Channel) == "" {
		return "", errors.New("预设缺少 OAuth 渠道，请重新保存")
	}
	return NormalizeOAuthChannel(snapshot.Channel)
}

// OAuthPresetSnapshot reads a consistent-enough configuration snapshot. Every
// subsequent apply is validated again against the current CPA model catalog.
func (k *Keys) oauthChannelPresetSnapshot(ctx context.Context, channel string) (OAuthChannelPreset, error) {
	channel, err := NormalizeOAuthChannel(channel)
	if err != nil {
		return OAuthChannelPreset{}, err
	}
	models, _, err := k.OAuthModelSettings(ctx, channel)
	if err != nil {
		return OAuthChannelPreset{}, err
	}
	aliases, _, err := k.OAuthModelAliases(ctx, channel)
	if err != nil {
		return OAuthChannelPreset{}, err
	}
	caps := map[string]string{}
	if channel == "codex" || channel == "antigravity" {
		caps, _, err = k.OAuthReasoningCaps(ctx, channel)
		if err != nil {
			return OAuthChannelPreset{}, err
		}
	}
	known := make(map[string]OAuthModelSetting, len(models))
	snapshot := OAuthChannelPreset{Channel: channel, ReasoningCaps: make(map[string]string)}
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model.ID))
		known[key] = model
		snapshot.Models = append(snapshot.Models, OAuthPresetModel{ID: model.ID, Enabled: model.Enabled, WildcardRule: model.WildcardRule})
		if model.Enabled {
			if cap := strings.ToLower(strings.TrimSpace(caps[key])); cap != "" {
				snapshot.ReasoningCaps[key] = cap
			}
		}
	}
	for _, alias := range aliases {
		if _, exists := known[strings.ToLower(strings.TrimSpace(alias.Name))]; exists {
			snapshot.Aliases = append(snapshot.Aliases, alias)
		}
	}
	canonicalizeOAuthPreset(&snapshot)
	return snapshot, nil
}

func canonicalizeOAuthPreset(snapshot *OAuthChannelPreset) {
	sort.Slice(snapshot.Models, func(i, j int) bool {
		return strings.ToLower(snapshot.Models[i].ID) < strings.ToLower(snapshot.Models[j].ID)
	})
	sort.Slice(snapshot.Aliases, func(i, j int) bool {
		left, right := strings.ToLower(snapshot.Aliases[i].Name), strings.ToLower(snapshot.Aliases[j].Name)
		if left != right {
			return left < right
		}
		return strings.ToLower(snapshot.Aliases[i].Alias) < strings.ToLower(snapshot.Aliases[j].Alias)
	})
}

// OAuthPresetSnapshotsEqual reports whether two snapshots describe the same
// portal-managed routing configuration. It intentionally ignores ordering and
// harmless casing differences in model and alias names.
func oauthChannelPresetsEqual(left, right OAuthChannelPreset) bool {
	leftChannel, leftErr := oauthPresetChannel(left)
	rightChannel, rightErr := oauthPresetChannel(right)
	if leftErr != nil || rightErr != nil || leftChannel != rightChannel {
		return false
	}
	canonicalizeOAuthPreset(&left)
	canonicalizeOAuthPreset(&right)
	if len(left.Models) != len(right.Models) || len(left.Aliases) != len(right.Aliases) {
		return false
	}
	for i := range left.Models {
		if !strings.EqualFold(strings.TrimSpace(left.Models[i].ID), strings.TrimSpace(right.Models[i].ID)) ||
			left.Models[i].Enabled != right.Models[i].Enabled ||
			!strings.EqualFold(strings.TrimSpace(left.Models[i].WildcardRule), strings.TrimSpace(right.Models[i].WildcardRule)) {
			return false
		}
	}
	for i := range left.Aliases {
		if !strings.EqualFold(strings.TrimSpace(left.Aliases[i].Name), strings.TrimSpace(right.Aliases[i].Name)) ||
			!strings.EqualFold(strings.TrimSpace(left.Aliases[i].Alias), strings.TrimSpace(right.Aliases[i].Alias)) ||
			left.Aliases[i].Fork != right.Aliases[i].Fork {
			return false
		}
	}
	leftCaps := normalizedReasoningCaps(left.ReasoningCaps)
	rightCaps := normalizedReasoningCaps(right.ReasoningCaps)
	if len(leftCaps) != len(rightCaps) {
		return false
	}
	for model, cap := range leftCaps {
		if rightCaps[model] != cap {
			return false
		}
	}
	return true
}

func normalizedReasoningCaps(caps map[string]string) map[string]string {
	normalized := make(map[string]string, len(caps))
	for model, cap := range caps {
		model = strings.ToLower(strings.TrimSpace(model))
		cap = strings.ToLower(strings.TrimSpace(cap))
		if model != "" && cap != "" {
			normalized[model] = cap
		}
	}
	return normalized
}

func (k *Keys) validateOAuthChannelPreset(ctx context.Context, snapshot OAuthChannelPreset) error {
	channel, err := oauthPresetChannel(snapshot)
	if err != nil {
		return err
	}
	models, _, err := k.OAuthModelSettings(ctx, channel)
	if err != nil {
		return err
	}
	if len(models) != len(snapshot.Models) {
		return errors.New("CPA 模型目录已变化，请重新保存该预设")
	}
	current := make(map[string]OAuthModelSetting, len(models))
	for _, model := range models {
		current[strings.ToLower(strings.TrimSpace(model.ID))] = model
	}
	target := make(map[string]OAuthPresetModel, len(snapshot.Models))
	for _, model := range snapshot.Models {
		key := strings.ToLower(strings.TrimSpace(model.ID))
		live, exists := current[key]
		if key == "" || !exists || target[key].ID != "" || !strings.EqualFold(strings.TrimSpace(live.WildcardRule), strings.TrimSpace(model.WildcardRule)) {
			return errors.New("CPA 模型目录或通配规则已变化，请重新保存该预设")
		}
		if model.WildcardRule != "" && model.Enabled {
			return fmt.Errorf("模型 %s 受通配规则控制，预设不能将其启用", live.ID)
		}
		target[key] = model
	}
	seenAliases := map[string]bool{}
	aliasCounts := map[string]int{}
	for _, alias := range snapshot.Aliases {
		owner := strings.ToLower(strings.TrimSpace(alias.Name))
		model, exists := target[owner]
		if !exists || !model.Enabled {
			return fmt.Errorf("预设中的别名 %s 所属模型不可用", strings.TrimSpace(alias.Alias))
		}
		name := strings.TrimSpace(alias.Alias)
		key := owner + "\x00" + strings.ToLower(name)
		aliasCounts[owner]++
		if len(name) > 128 || !oauthAliasIDPattern.MatchString(name) || seenAliases[key] || aliasCounts[owner] > 32 {
			return fmt.Errorf("预设中的模型 %s 别名无效或重复", alias.Name)
		}
		seenAliases[key] = true
	}
	settings := make([]OAuthModelSetting, 0, len(models))
	for _, live := range models {
		live.Enabled = target[strings.ToLower(live.ID)].Enabled
		settings = append(settings, live)
	}
	if err := validateOAuthAliasNames(settings, snapshot.Aliases); err != nil {
		return fmt.Errorf("预设中的模型别名无效：%w", err)
	}
	if channel != "codex" && channel != "antigravity" && len(snapshot.ReasoningCaps) > 0 {
		return errors.New("该渠道暂不支持思考强度上限")
	}
	for id, cap := range snapshot.ReasoningCaps {
		key := strings.ToLower(strings.TrimSpace(id))
		live, exists := current[key]
		if !exists || !target[key].Enabled || len(OAuthReasoningLevels(channel, live)) == 0 || strings.EqualFold(cap, "ultra") || reasoningEffortIndex(strings.ToLower(cap)) < 0 || !containsReasoningLevel(OAuthReasoningLevels(channel, live), cap) {
			return fmt.Errorf("预设中的模型 %s 不支持思考强度 %s", id, cap)
		}
	}
	return nil
}

func (k *Keys) applyOAuthChannelPreset(ctx context.Context, snapshot OAuthChannelPreset) error {
	channel, err := oauthPresetChannel(snapshot)
	if err != nil {
		return err
	}
	models, _, err := k.OAuthModelSettings(ctx, channel)
	if err != nil {
		return err
	}
	// Clear every known model mapping first so target model IDs cannot collide
	// with aliases belonging to a model that will be disabled or enabled.
	_, aliasRevision, err := k.OAuthModelAliases(ctx, channel)
	if err != nil {
		return err
	}
	clearInputs := make([]OAuthModelAliasInput, 0, len(models))
	for _, model := range models {
		if model.Enabled {
			clearInputs = append(clearInputs, OAuthModelAliasInput{Model: model.ID, KeepOriginal: true})
		}
	}
	// ApplyOAuthPreset holds presetMu and has checked the final target state.
	// Intermediate clear/enable steps and rollback may temporarily expose names
	// that the final alias settings hide; do not validate those transient states.
	if err := k.setOAuthModelAliases(ctx, clearInputs, aliasRevision, false, channel); err != nil {
		return err
	}

	target := make(map[string]OAuthPresetModel, len(snapshot.Models))
	for _, model := range snapshot.Models {
		target[strings.ToLower(strings.TrimSpace(model.ID))] = model
	}
	for _, model := range models {
		if model.Enabled && !target[strings.ToLower(model.ID)].Enabled {
			if err := k.setOAuthModelEnabled(ctx, model.ID, false, false, channel); err != nil {
				return err
			}
		}
	}
	models, _, err = k.OAuthModelSettings(ctx, channel)
	if err != nil {
		return err
	}
	for _, model := range models {
		if !model.Enabled && target[strings.ToLower(model.ID)].Enabled {
			if err := k.setOAuthModelEnabled(ctx, model.ID, true, false, channel); err != nil {
				return err
			}
		}
	}

	models, _, err = k.OAuthModelSettings(ctx, channel)
	if err != nil {
		return err
	}
	aliasesByModel := make(map[string][]cpamp.OAuthModelAlias)
	for _, alias := range snapshot.Aliases {
		key := strings.ToLower(strings.TrimSpace(alias.Name))
		aliasesByModel[key] = append(aliasesByModel[key], alias)
	}
	inputs := make([]OAuthModelAliasInput, 0, len(models))
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		entries := aliasesByModel[strings.ToLower(model.ID)]
		aliases := make([]string, 0, len(entries))
		keepOriginal := len(entries) == 0
		for _, entry := range entries {
			alias := strings.TrimSpace(entry.Alias)
			if alias != "" && !strings.EqualFold(alias, model.ID) {
				aliases = append(aliases, alias)
			}
			keepOriginal = keepOriginal || entry.Fork
		}
		inputs = append(inputs, OAuthModelAliasInput{Model: model.ID, Aliases: strings.Join(aliases, ","), KeepOriginal: keepOriginal})
	}
	_, aliasRevision, err = k.OAuthModelAliases(ctx, channel)
	if err != nil {
		return err
	}
	if err := k.setOAuthModelAliases(ctx, inputs, aliasRevision, false, channel); err != nil {
		return err
	}

	if channel != "codex" && channel != "antigravity" {
		return nil
	}
	_, reasoningRevision, err := k.OAuthReasoningCaps(ctx, channel)
	if err != nil {
		return err
	}
	reasoningInputs := make([]ReasoningCapInput, 0, len(models))
	for _, model := range models {
		if model.Enabled && len(OAuthReasoningLevels(channel, model)) > 0 {
			reasoningInputs = append(reasoningInputs, ReasoningCapInput{Model: model.ID, Cap: snapshot.ReasoningCaps[strings.ToLower(model.ID)]})
		}
	}
	return k.setOAuthReasoningCaps(ctx, reasoningInputs, reasoningRevision, channel)
}
