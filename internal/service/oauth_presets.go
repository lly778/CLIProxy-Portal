package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
)

const OAuthPresetVersion = 1

// OAuthPresetSnapshot is the portable, credential-free part of Codex routing
// managed by the portal. It deliberately excludes OAuth credentials.
type OAuthPresetSnapshot struct {
	Version       int                     `json:"version"`
	Models        []OAuthPresetModel      `json:"models"`
	Aliases       []cpamp.OAuthModelAlias `json:"aliases,omitempty"`
	ReasoningCaps map[string]string       `json:"reasoning_caps,omitempty"`
}

type OAuthPresetModel struct {
	ID                string `json:"id"`
	Enabled           bool   `json:"enabled"`
	WildcardRule      string `json:"wildcard_rule,omitempty"`
	SupportsReasoning bool   `json:"supports_reasoning,omitempty"`
}

// OAuthPresetSnapshot reads a consistent-enough configuration snapshot. Every
// subsequent apply is validated again against the current CPA model catalog.
func (k *Keys) OAuthPresetSnapshot(ctx context.Context) (OAuthPresetSnapshot, error) {
	models, _, err := k.OAuthModelSettings(ctx)
	if err != nil {
		return OAuthPresetSnapshot{}, err
	}
	aliases, _, err := k.OAuthModelAliases(ctx)
	if err != nil {
		return OAuthPresetSnapshot{}, err
	}
	caps, _, err := k.OAuthReasoningCaps(ctx)
	if err != nil {
		return OAuthPresetSnapshot{}, err
	}
	known := make(map[string]OAuthModelSetting, len(models))
	snapshot := OAuthPresetSnapshot{Version: OAuthPresetVersion, ReasoningCaps: make(map[string]string)}
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model.ID))
		known[key] = model
		snapshot.Models = append(snapshot.Models, OAuthPresetModel{ID: model.ID, Enabled: model.Enabled, WildcardRule: model.WildcardRule, SupportsReasoning: len(model.ThinkingLevels) > 0})
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

func canonicalizeOAuthPreset(snapshot *OAuthPresetSnapshot) {
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

// ApplyOAuthPreset replaces the three portal-managed routing areas together.
// If an operation fails, it makes a best-effort rollback to the snapshot read
// immediately before applying the preset.
func (k *Keys) ApplyOAuthPreset(ctx context.Context, requested OAuthPresetSnapshot) error {
	k.presetMu.Lock()
	defer k.presetMu.Unlock()

	current, err := k.OAuthPresetSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("读取当前调用配置失败：%w", err)
	}
	if err := k.validateOAuthPreset(ctx, requested); err != nil {
		return err
	}
	if err := k.applyOAuthPreset(ctx, requested); err != nil {
		if rollbackErr := k.applyOAuthPreset(ctx, current); rollbackErr != nil {
			return fmt.Errorf("应用预设失败：%v；恢复原配置也失败：%w", err, rollbackErr)
		}
		return fmt.Errorf("应用预设失败，已恢复原配置：%w", err)
	}
	return nil
}

func (k *Keys) validateOAuthPreset(ctx context.Context, snapshot OAuthPresetSnapshot) error {
	if snapshot.Version != OAuthPresetVersion {
		return errors.New("预设版本不受支持")
	}
	models, _, err := k.OAuthModelSettings(ctx)
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
	for _, alias := range snapshot.Aliases {
		owner := strings.ToLower(strings.TrimSpace(alias.Name))
		model, exists := target[owner]
		if !exists || !model.Enabled {
			return fmt.Errorf("预设中的别名 %s 所属模型不可用", strings.TrimSpace(alias.Alias))
		}
	}
	settings := make([]OAuthModelSetting, 0, len(models))
	for _, live := range models {
		live.Enabled = target[strings.ToLower(live.ID)].Enabled
		settings = append(settings, live)
	}
	if err := validateOAuthAliasNames(settings, snapshot.Aliases); err != nil {
		return fmt.Errorf("预设中的模型别名无效：%w", err)
	}
	for id, cap := range snapshot.ReasoningCaps {
		key := strings.ToLower(strings.TrimSpace(id))
		live, exists := current[key]
		if !exists || !target[key].Enabled || len(live.ThinkingLevels) == 0 || strings.EqualFold(cap, "ultra") || reasoningEffortIndex(strings.ToLower(cap)) < 0 || !containsReasoningLevel(live.ThinkingLevels, cap) {
			return fmt.Errorf("预设中的模型 %s 不支持思考强度 %s", id, cap)
		}
	}
	return nil
}

func (k *Keys) applyOAuthPreset(ctx context.Context, snapshot OAuthPresetSnapshot) error {
	models, _, err := k.OAuthModelSettings(ctx)
	if err != nil {
		return err
	}
	// Clear every known model mapping first so target model IDs cannot collide
	// with aliases belonging to a model that will be disabled or enabled.
	_, aliasRevision, err := k.OAuthModelAliases(ctx)
	if err != nil {
		return err
	}
	clearInputs := make([]OAuthModelAliasInput, 0, len(models))
	for _, model := range models {
		if model.Enabled {
			clearInputs = append(clearInputs, OAuthModelAliasInput{Model: model.ID, KeepOriginal: true})
		}
	}
	if err := k.SetOAuthModelAliases(ctx, clearInputs, aliasRevision); err != nil {
		return err
	}

	target := make(map[string]OAuthPresetModel, len(snapshot.Models))
	for _, model := range snapshot.Models {
		target[strings.ToLower(strings.TrimSpace(model.ID))] = model
	}
	for _, model := range models {
		if model.Enabled && !target[strings.ToLower(model.ID)].Enabled {
			if err := k.SetOAuthModelEnabled(ctx, model.ID, false); err != nil {
				return err
			}
		}
	}
	models, _, err = k.OAuthModelSettings(ctx)
	if err != nil {
		return err
	}
	for _, model := range models {
		if !model.Enabled && target[strings.ToLower(model.ID)].Enabled {
			if err := k.SetOAuthModelEnabled(ctx, model.ID, true); err != nil {
				return err
			}
		}
	}

	models, _, err = k.OAuthModelSettings(ctx)
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
	_, aliasRevision, err = k.OAuthModelAliases(ctx)
	if err != nil {
		return err
	}
	if err := k.SetOAuthModelAliases(ctx, inputs, aliasRevision); err != nil {
		return err
	}

	_, reasoningRevision, err := k.OAuthReasoningCaps(ctx)
	if err != nil {
		return err
	}
	reasoningInputs := make([]ReasoningCapInput, 0, len(models))
	for _, model := range models {
		if model.Enabled && len(model.ThinkingLevels) > 0 {
			reasoningInputs = append(reasoningInputs, ReasoningCapInput{Model: model.ID, Cap: snapshot.ReasoningCaps[strings.ToLower(model.ID)]})
		}
	}
	return k.SetOAuthReasoningCaps(ctx, reasoningInputs, reasoningRevision)
}
