package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
)

// Call only after the final configuration is written, while holding presetMu.
// Reapplying disabled=false invokes CPA's post-persist registration hook without
// toggling the credential off or refreshing its OAuth token.
func (k *Keys) refreshOAuthRegistrations(ctx context.Context, channels ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reader, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	})
	if !ok {
		return errors.New("CPAMP 客户端不支持读取上游账号")
	}
	files, err := reader.ListAuthFiles(ctx)
	if err != nil {
		return fmt.Errorf("读取上游账号失败：%w", err)
	}
	selected := map[string]bool{}
	for _, channel := range channels {
		selected[channel] = true
	}
	active := []cpamp.AuthFile{}
	seen := map[string]bool{}
	for _, file := range files {
		channel := strings.ToLower(strings.TrimSpace(file.Provider))
		identity := upstreamAccountID(file)
		if selected[channel] && !file.Disabled && !seen[identity] {
			active = append(active, file)
			seen[identity] = true
		}
	}
	if len(active) == 0 {
		return nil
	}
	client, ok := k.CPAMP.(interface {
		SetAuthFileDisabled(context.Context, cpamp.AuthFile, bool) error
		ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error)
	})
	if !ok {
		return errors.New("CPAMP 客户端不支持刷新及读取 CPA 模型注册")
	}
	snapshots := map[string]OAuthChannelPreset{}
	for _, file := range active {
		channel := strings.ToLower(strings.TrimSpace(file.Provider))
		if _, exists := snapshots[channel]; !exists {
			models, _, err := k.OAuthModelSettings(ctx, channel)
			if err != nil {
				return fmt.Errorf("读取 %s 注册配置失败：%w", OAuthChannelLabel(channel), err)
			}
			aliases, _, err := k.OAuthModelAliases(ctx, channel)
			if err != nil {
				return fmt.Errorf("读取 %s 别名配置失败：%w", OAuthChannelLabel(channel), err)
			}
			snapshot := OAuthChannelPreset{Channel: channel, Aliases: aliases}
			for _, model := range models {
				snapshot.Models = append(snapshot.Models, OAuthPresetModel{ID: model.ID, Enabled: model.Enabled})
			}
			snapshots[channel] = snapshot
		}
	}
	for _, file := range active {
		channel := strings.ToLower(strings.TrimSpace(file.Provider))
		before, err := client.ListAuthFileModels(ctx, file)
		if err != nil {
			return fmt.Errorf("读取 %s 已注册模型失败：%w", OAuthChannelLabel(channel), err)
		}
		for attempt := 0; ; attempt++ {
			// CPA reloads configuration asynchronously. Retry a stale projection,
			// but re-resolve the identity and disabled state before each write.
			current, err := reader.ListAuthFiles(ctx)
			if err != nil {
				return fmt.Errorf("确认上游账号状态失败：%w", err)
			}
			found, disabled := false, false
			for _, candidate := range current {
				if upstreamAccountID(candidate) == upstreamAccountID(file) {
					file, found, disabled = candidate, true, candidate.Disabled
					break
				}
			}
			if !found {
				return errors.New("上游账号已发生变化，请刷新页面后重试")
			}
			if disabled {
				break
			}
			if err := client.SetAuthFileDisabled(ctx, file, false); err != nil {
				return fmt.Errorf("刷新 %s 账号注册失败：%w", OAuthChannelLabel(channel), err)
			}
			after, err := client.ListAuthFileModels(ctx, file)
			if err != nil {
				return fmt.Errorf("确认 %s 模型注册失败：%w", OAuthChannelLabel(channel), err)
			}
			if err = verifyOAuthRegistration(snapshots[channel], before, after); err == nil {
				break
			}
			if attempt == 3 {
				return fmt.Errorf("%s 模型注册未同步：%w", OAuthChannelLabel(channel), err)
			}
			before = append(before, after...)
			timer := time.NewTimer(time.Duration(150*(1<<attempt)) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}

// Account plans can expose different subsets of the catalog. Require mappings
// for targets this account demonstrably supports, rather than guessing that
// every statically defined model must be available to every credential.
func verifyOAuthRegistration(snapshot OAuthChannelPreset, before, after []cpamp.Model) error {
	has := func(models []cpamp.Model, name string) bool {
		for _, model := range models {
			if strings.EqualFold(model.ID, name) || strings.HasSuffix(strings.ToLower(model.ID), "/"+strings.ToLower(name)) {
				return true
			}
		}
		return false
	}
	aliases := map[string][]cpamp.OAuthModelAlias{}
	known := map[string]bool{}
	for _, model := range snapshot.Models {
		known[strings.ToLower(model.ID)] = true
	}
	visible := map[string]bool{}
	for _, alias := range snapshot.Aliases {
		key := strings.ToLower(alias.Name)
		aliases[key] = append(aliases[key], alias)
		if !known[key] {
			visible[strings.ToLower(alias.Alias)] = true
		}
	}
	for _, model := range snapshot.Models {
		if !model.Enabled {
			continue
		}
		entries := aliases[strings.ToLower(model.ID)]
		keep := len(entries) == 0
		for _, alias := range entries {
			visible[strings.ToLower(alias.Alias)] = true
			keep = keep || alias.Fork
		}
		if keep {
			visible[strings.ToLower(model.ID)] = true
		}
		supported := has(before, model.ID) || has(after, model.ID)
		for _, alias := range entries {
			supported = supported || has(before, alias.Alias) || has(after, alias.Alias)
		}
		if !supported {
			continue
		}
		for _, alias := range entries {
			if !has(after, alias.Alias) {
				return fmt.Errorf("别名 %s 尚未注册", alias.Alias)
			}
		}
		if keep && !has(after, model.ID) {
			return fmt.Errorf("模型 %s 尚未注册", model.ID)
		}
	}
	for _, model := range snapshot.Models {
		if has(after, model.ID) && !visible[strings.ToLower(model.ID)] {
			return fmt.Errorf("模型 %s 的旧路由尚未移除", model.ID)
		}
	}
	return nil
}
