package service

import (
	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"context"
	"errors"
	"strconv"
	"strings"
)

func (k *Keys) agyDefinitions(ctx context.Context) ([]cpamp.OAuthModelDefinition, error) {
	s, e := k.Store.AGYSettings(ctx)
	if e != nil {
		return nil, e
	}
	out := []cpamp.OAuthModelDefinition{}
	for _, m := range s.Models {
		out = append(out, cpamp.OAuthModelDefinition{ID: agy.Prefix + m.ID, DisplayName: m.Name, ThinkingLevels: append([]string(nil), m.Levels...)})
	}
	return out, nil
}
func (k *Keys) agyModelSettings(ctx context.Context) ([]OAuthModelSetting, []string, error) {
	s, e := k.Store.AGYSettings(ctx)
	if e != nil {
		return nil, nil, e
	}
	rules := map[string]agy.ModelRule{}
	for _, r := range s.Rules {
		rules[r.ID] = r
	}
	out := []OAuthModelSetting{}
	for _, m := range s.Models {
		out = append(out, OAuthModelSetting{ID: agy.Prefix + m.ID, DisplayName: m.Name, ThinkingLevels: append([]string(nil), m.Levels...), Enabled: rules[m.ID].Enabled})
	}
	return out, nil, nil
}
func (k *Keys) agyAliases(ctx context.Context) ([]cpamp.OAuthModelAlias, string, error) {
	s, e := k.Store.AGYSettings(ctx)
	if e != nil {
		return nil, "", e
	}
	out := []cpamp.OAuthModelAlias{}
	for _, r := range s.Rules {
		if !r.Enabled {
			continue
		}
		for _, alias := range r.AllAliases() {
			out = append(out, cpamp.OAuthModelAlias{Name: agy.Prefix + r.ID, Alias: agy.Prefix + alias, Fork: !r.HideOriginal})
		}
	}
	return out, "agy:" + strconv.FormatInt(s.Revision, 10), nil
}
func (k *Keys) agyCaps(ctx context.Context) (map[string]string, string, error) {
	s, e := k.Store.AGYSettings(ctx)
	if e != nil {
		return nil, "", e
	}
	out := map[string]string{}
	for _, r := range s.Rules {
		if r.Enabled && r.Cap != "" {
			out[strings.ToLower(agy.Prefix+r.ID)] = r.Cap
		}
	}
	return out, "agy:" + strconv.FormatInt(s.Revision, 10), nil
}
func (k *Keys) applyAGYChannel(ctx context.Context, snapshot OAuthChannelPreset) error {
	canonicalizeOAuthPreset(&snapshot)
	s, e := k.Store.AGYSettings(ctx)
	if e != nil {
		return e
	}
	expected := s.Revision
	old := map[string]agy.ModelRule{}
	for _, r := range s.Rules {
		old[r.ID] = r
	}
	if len(snapshot.Models) != len(s.Models) {
		return errors.New("AGY 模型目录已变化，请刷新配置")
	}
	known := map[string]bool{}
	for _, m := range s.Models {
		known[agy.Prefix+m.ID] = true
	}
	rules := map[string]agy.ModelRule{}
	for _, m := range snapshot.Models {
		id := strings.TrimPrefix(m.ID, agy.Prefix)
		if !known[m.ID] || rules[id].ID != "" {
			return errors.New("预设包含未知 AGY 模型")
		}
		r := old[id]
		r.ID = id
		r.Enabled = m.Enabled
		r.Alias = ""
		r.Aliases = nil
		r.HideOriginal = false
		r.Cap = ""
		if m.Enabled {
			r.Cap = snapshot.ReasoningCaps[strings.ToLower(m.ID)]
		}
		rules[id] = r
	}
	for _, a := range snapshot.Aliases {
		id := strings.TrimPrefix(a.Name, agy.Prefix)
		r, ok := rules[id]
		if !ok || !r.Enabled || !strings.HasPrefix(a.Alias, agy.Prefix) {
			return errors.New("AGY 别名目标无效")
		}
		if len(r.Aliases) >= 32 {
			return errors.New("单个模型最多设置 32 个别名")
		}
		r.Aliases = append(r.Aliases, strings.TrimPrefix(a.Alias, agy.Prefix))
		if len(r.Aliases) == 1 {
			r.HideOriginal = !a.Fork
		} else {
			r.HideOriginal = r.HideOriginal && !a.Fork
		}
		rules[id] = r
	}
	s.Rules = nil
	for _, m := range s.Models {
		s.Rules = append(s.Rules, rules[m.ID])
	}
	return k.Store.SaveAGYSettings(ctx, s, expected)
}
