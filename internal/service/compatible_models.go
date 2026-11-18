package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"gopkg.in/yaml.v3"
)

const compatibleChannelPrefix = "compat-"

func IsCompatibleChannel(channel string) bool {
	id, ok := strings.CutPrefix(channel, compatibleChannelPrefix)
	if !ok || len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

type compatibleModel struct {
	node      *yaml.Node
	id, alias string
	levels    []string
}

type compatibleProvider struct {
	node     *yaml.Node
	label    string
	disabled bool
	deepSeek bool
	models   []compatibleModel
}

type compatibleConfigReader interface {
	GetConfigYAML(context.Context) ([]byte, error)
}

func parseCompatibleModel(node *yaml.Node) (compatibleModel, error) {
	var fields struct {
		Name     string
		Alias    string
		Thinking struct{ Levels []string }
	}
	if node.Kind != yaml.MappingNode || node.Decode(&fields) != nil ||
		!domain.ValidClientModelID(fields.Name) || (fields.Alias != "" && !domain.ValidClientModelID(fields.Alias)) {
		return compatibleModel{}, errors.New("第三方模型名称或别名无效")
	}
	return compatibleModel{node: node, id: fields.Name, alias: fields.Alias, levels: fields.Thinking.Levels}, nil
}

func parseCompatibleProviders(doc *yaml.Node) (map[string]compatibleProvider, error) {
	section, err := yamlField(doc.Content[0], "openai-compatibility")
	providers := make(map[string]compatibleProvider)
	if err != nil || section == nil || section.Tag == "!!null" {
		return providers, err
	}
	if section.Kind != yaml.SequenceNode {
		return nil, errors.New("第三方供应商配置格式无效")
	}
	seen := make(map[string]bool)
	for _, node := range section.Content {
		name, nameErr := yamlField(node, "name")
		base, baseErr := yamlField(node, "base-url")
		prefix, prefixErr := yamlField(node, "prefix")
		models, modelErr := yamlField(node, "models")
		disabled, disabledErr := yamlField(node, "disabled")
		if nameErr != nil || baseErr != nil || prefixErr != nil || modelErr != nil || disabledErr != nil {
			return nil, errors.New("第三方供应商配置格式无效")
		}
		if name == nil || strings.TrimSpace(name.Value) == "" || base == nil || models == nil || models.Kind != yaml.SequenceNode || len(models.Content) == 0 {
			continue // CPA requires explicit model mappings for this editor.
		}
		routingPrefix := ""
		if prefix != nil {
			routingPrefix = prefix.Value
		}
		channel := compatibleChannelPrefix + security.SHA256(name.Value + "\x00" + base.Value + "\x00" + routingPrefix)[:32]
		if seen[channel] {
			delete(providers, channel)
			continue
		}
		seen[channel] = true
		address, _ := url.Parse(base.Value)
		p := compatibleProvider{
			node: node, label: name.Value,
			disabled: disabled != nil && strings.EqualFold(disabled.Value, "true"),
			deepSeek: address != nil && strings.EqualFold(address.Hostname(), "api.deepseek.com"),
		}
		for _, item := range models.Content {
			model, err := parseCompatibleModel(item)
			if err != nil {
				p.models = nil
				break
			}
			p.models = append(p.models, model)
		}
		if len(p.models) > 0 {
			providers[channel] = p
		}
	}
	return providers, nil
}

func (k *Keys) compatibleProviders(ctx context.Context) (map[string]compatibleProvider, error) {
	client, ok := k.CPAMP.(compatibleConfigReader)
	if !ok {
		return nil, nil
	}
	data, err := client.GetConfigYAML(ctx)
	if err != nil {
		return nil, err
	}
	doc, _, err := parseReasoningConfig(data)
	if err != nil {
		return nil, err
	}
	return parseCompatibleProviders(doc)
}

func (k *Keys) CompatibleChannelLabels(ctx context.Context) (map[string]string, error) {
	providers, err := k.compatibleProviders(ctx)
	labels := make(map[string]string, len(providers))
	for channel, p := range providers {
		labels[channel] = p.label
	}
	return labels, err
}

func (k *Keys) readCompatibleCaps(ctx context.Context, channel string) (map[string]string, string, error) {
	client, ok := k.CPAMP.(compatibleConfigReader)
	if !ok {
		return nil, "", errors.New("管理服务不支持第三方模型配置")
	}
	data, err := client.GetConfigYAML(ctx)
	if err != nil {
		return nil, "", err
	}
	doc, rules, err := parseReasoningConfig(data)
	if err != nil {
		return nil, "", err
	}
	providers, err := parseCompatibleProviders(doc)
	if err != nil {
		return nil, "", err
	}
	p, found := providers[channel]
	if !found {
		return nil, "", errors.New("第三方供应商配置已变化，请重新打开弹窗")
	}
	caps, _ := compatibleReasoningCaps(rules, p.models)
	return caps, security.SHA256(string(data)), nil
}

func (k *Keys) compatibleModelGroups(ctx context.Context, channel string, live compatibleProvider) (map[string][]compatibleModel, error) {
	byModel := map[string][]compatibleModel{}
	if k.Store != nil {
		archived, err := k.Store.CompatibleModelCatalog(ctx, channel)
		if err != nil {
			return nil, err
		}
		for _, raw := range archived {
			var node yaml.Node
			if yaml.Unmarshal([]byte(raw), &node) != nil || len(node.Content) != 1 {
				return nil, errors.New("已停用的第三方模型格式无效")
			}
			model, err := parseCompatibleModel(node.Content[0])
			if err != nil {
				return nil, err
			}
			id := strings.ToLower(model.id)
			byModel[id] = append(byModel[id], model)
		}
	}
	active := map[string]bool{}
	for _, model := range live.models {
		id := strings.ToLower(model.id)
		if !active[id] {
			byModel[id] = nil
			active[id] = true
		}
		byModel[id] = append(byModel[id], model)
	}
	return byModel, nil
}

func compatibleMappings(p compatibleProvider) ([]cpamp.OAuthModelAlias, map[string]bool) {
	aliases := []cpamp.OAuthModelAlias{}
	active := map[string]bool{}
	original := map[string]bool{}
	if p.disabled {
		return aliases, active
	}
	seen := map[[2]string]bool{}
	for _, model := range p.models {
		id := strings.ToLower(model.id)
		active[id] = true
		if model.alias == "" || strings.EqualFold(model.alias, model.id) {
			original[id] = true
		}
	}
	for _, model := range p.models {
		id := strings.ToLower(model.id)
		alias := strings.ToLower(model.alias)
		if alias == "" || alias == id || seen[[2]string{id, alias}] {
			continue
		}
		seen[[2]string{id, alias}] = true
		aliases = append(aliases, cpamp.OAuthModelAlias{Name: model.id, Alias: model.alias, Fork: original[id]})
	}
	return aliases, active
}

func (k *Keys) compatibleModelSettings(ctx context.Context, channel string) ([]OAuthModelSetting, []cpamp.OAuthModelAlias, error) {
	providers, err := k.compatibleProviders(ctx)
	if err != nil {
		return nil, nil, err
	}
	p, ok := providers[channel]
	if !ok {
		return nil, nil, errors.New("第三方供应商配置已变化，请重新打开弹窗")
	}
	groups, err := k.compatibleModelGroups(ctx, channel, p)
	if err != nil {
		return nil, nil, err
	}
	aliases, active := compatibleMappings(p)
	models := make([]OAuthModelSetting, 0, len(groups))
	for id, mappings := range groups {
		var levels []string
		if compatibleCapIsolated(providers, channel, id) {
			levels = mappings[0].levels
			if p.deepSeek && len(levels) == 0 {
				switch id {
				case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-pro":
					levels = []string{"low", "high", "max"}
				}
			}
		}
		models = append(models, OAuthModelSetting{ID: mappings[0].id, Enabled: active[id], ThinkingLevels: levels})
	}
	sort.Slice(models, func(i, j int) bool { return ModelIDLess(models[i].ID, models[j].ID) })
	return models, aliases, nil
}

// CPA matches payload rules by upstream model name as well as requested alias.
// An OpenAI-format rule must not affect a second provider with the same name.
func compatibleCapIsolated(providers map[string]compatibleProvider, channel, id string) bool {
	for candidate, p := range providers {
		if candidate == channel {
			continue
		}
		for _, model := range p.models {
			if strings.EqualFold(model.id, id) || strings.EqualFold(model.alias, id) {
				return false
			}
		}
	}
	return true
}

// The match uses the translated model or the requested alias, so also check
// aliases that are being introduced in the same multi-channel edit.
func validateCompatibleCapScope(snapshot OAuthPresetSnapshot) error {
	type modelRoute struct{ channel, model string }
	names := map[string][]modelRoute{}
	for _, entry := range snapshot.Channels {
		if !IsCompatibleChannel(entry.Channel) {
			continue
		}
		for _, model := range entry.Models {
			if model.Enabled {
				id := strings.ToLower(strings.TrimSpace(model.ID))
				names[id] = append(names[id], modelRoute{entry.Channel, id})
			}
		}
		for _, alias := range entry.Aliases {
			name := strings.ToLower(strings.TrimSpace(alias.Alias))
			id := strings.ToLower(strings.TrimSpace(alias.Name))
			names[name] = append(names[name], modelRoute{entry.Channel, id})
		}
	}
	for _, entry := range snapshot.Channels {
		if !IsCompatibleChannel(entry.Channel) {
			continue
		}
		for id := range normalizedReasoningCaps(entry.ReasoningCaps) {
			for _, route := range names[id] {
				if route.channel != entry.Channel || route.model != id {
					return fmt.Errorf("模型 %s 的思考强度上限与另一个第三方模型或别名冲突", id)
				}
			}
		}
	}
	return nil
}

func (k *Keys) applyCompatibleChannel(ctx context.Context, snapshot OAuthChannelPreset) error {
	k.modelConfigMu.Lock()
	defer k.modelConfigMu.Unlock()
	snapshot.ReasoningCaps = normalizedReasoningCaps(snapshot.ReasoningCaps)
	client, ok := k.CPAMP.(interface {
		compatibleConfigReader
		PutConfigYAML(context.Context, []byte) error
	})
	if !ok {
		return errors.New("管理服务不支持第三方模型配置")
	}
	data, err := client.GetConfigYAML(ctx)
	if err != nil {
		return err
	}
	doc, rules, err := parseReasoningConfig(data)
	if err != nil {
		return err
	}
	providers, err := parseCompatibleProviders(doc)
	if err != nil {
		return err
	}
	p, found := providers[snapshot.Channel]
	if !found {
		return errors.New("第三方供应商配置已变化，请重新打开弹窗")
	}
	groups, err := k.compatibleModelGroups(ctx, snapshot.Channel, p)
	if err != nil {
		return err
	}
	mappings, archive, err := compatibleTargetMappings(snapshot, groups)
	if err != nil {
		return err
	}
	if len(mappings) > 0 {
		models, _ := yamlField(p.node, "models")
		models.Content = mappings
	}
	setCompatibleYAMLField(p.node, "disabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(len(mappings) == 0)})
	if err := updateCompatibleReasoning(doc, rules, snapshot, providers, groups); err != nil {
		return err
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if encoder.Encode(doc) != nil || encoder.Close() != nil {
		return errors.New("无法编码第三方模型配置")
	}
	latest, err := client.GetConfigYAML(ctx)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, data) {
		return ErrOAuthConfigurationChanged
	}
	// Stage the new archive without dropping models which are about to be
	// enabled. A failed write must leave both catalogs recoverable.
	if k.Store != nil {
		before, err := k.Store.CompatibleModelCatalog(ctx, snapshot.Channel)
		if err != nil {
			return err
		}
		staged, err := mergeCompatibleArchives(before, archive)
		if err != nil {
			return err
		}
		if err := k.Store.SaveCompatibleModelCatalog(ctx, snapshot.Channel, staged); err != nil {
			return errors.New("无法保存第三方模型目录")
		}
	} else if len(archive) != 0 {
		return errors.New("第三方模型目录不可用")
	}
	if err := client.PutConfigYAML(ctx, out.Bytes()); err != nil {
		return err
	}
	if err := confirmCompatibleConfig(ctx, client, snapshot, mappings); err != nil {
		return err
	}
	if k.Store != nil {
		return k.Store.SaveCompatibleModelCatalog(ctx, snapshot.Channel, archive)
	}
	return nil
}

func mergeCompatibleArchives(previous, next []string) ([]string, error) {
	groups := map[string][]string{}
	for _, archive := range [][]string{previous, next} {
		entries := map[string][]string{}
		for _, raw := range archive {
			var data struct{ Name string }
			if err := yaml.Unmarshal([]byte(raw), &data); err != nil || !domain.ValidClientModelID(data.Name) {
				return nil, errors.New("已停用的第三方模型格式无效")
			}
			id := strings.ToLower(data.Name)
			entries[id] = append(entries[id], raw)
		}
		for id, rows := range entries {
			groups[id] = rows
		}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var merged []string
	for _, id := range ids {
		merged = append(merged, groups[id]...)
	}
	return merged, nil
}

func compatibleTargetMappings(snapshot OAuthChannelPreset, groups map[string][]compatibleModel) ([]*yaml.Node, []string, error) {
	aliases := make(map[string][]cpamp.OAuthModelAlias)
	for _, alias := range snapshot.Aliases {
		key := strings.ToLower(strings.TrimSpace(alias.Name))
		alias.Alias = strings.TrimSpace(alias.Alias)
		aliases[key] = append(aliases[key], alias)
	}
	var active []*yaml.Node
	var archived []string
	seen := make(map[string]bool, len(groups))
	if len(snapshot.Models) != len(groups) {
		return nil, nil, ErrOAuthConfigurationChanged
	}
	for _, row := range snapshot.Models {
		key := strings.ToLower(strings.TrimSpace(row.ID))
		group := groups[key]
		if len(group) == 0 || seen[key] {
			return nil, nil, ErrOAuthConfigurationChanged
		}
		seen[key] = true
		if !row.Enabled {
			for _, model := range group {
				// Decode the model alone and omit comments from the archive.
				var fields map[string]any
				if model.node.Decode(&fields) != nil {
					return nil, nil, errors.New("无法保留已停用的第三方模型定义")
				}
				data, err := yaml.Marshal(fields)
				if err != nil || len(data) > 64<<10 {
					return nil, nil, errors.New("无法保留已停用的第三方模型定义")
				}
				archived = append(archived, string(data))
			}
			continue
		}
		names := aliases[key]
		keep := len(names) == 0
		for _, alias := range names {
			keep = keep || alias.Fork
			if !strings.EqualFold(alias.Alias, row.ID) {
				active = append(active, compatibleMappingNode(group, alias.Alias))
			} else {
				keep = true
			}
		}
		if keep {
			active = append(active, compatibleMappingNode(group, group[0].id))
		}
	}
	return active, archived, nil
}

func compatibleMappingNode(group []compatibleModel, alias string) *yaml.Node {
	source := group[0]
	for _, model := range group {
		if strings.EqualFold(model.alias, alias) || model.alias == "" && strings.EqualFold(model.id, alias) {
			source = model
			break
		}
	}
	node := *source.node
	node.Content = append([]*yaml.Node(nil), node.Content...)
	setCompatibleYAMLField(&node, "alias", yamlScalar(alias))
	return &node
}

func setCompatibleYAMLField(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, yamlScalar(key), value)
}

func confirmCompatibleConfig(ctx context.Context, client compatibleConfigReader, snapshot OAuthChannelPreset, mappings []*yaml.Node) error {
	data, err := client.GetConfigYAML(ctx)
	if err != nil {
		return err
	}
	doc, rules, err := parseReasoningConfig(data)
	if err != nil {
		return err
	}
	providers, err := parseCompatibleProviders(doc)
	if err != nil {
		return err
	}
	actual, found := providers[snapshot.Channel]
	if !found || actual.disabled != (len(mappings) == 0) || !actual.disabled && len(actual.models) != len(mappings) {
		return errors.New("CPA 未确认第三方模型启用状态")
	}
	for i, mapping := range mappings {
		expected, _ := parseCompatibleModel(mapping)
		if expected.id != actual.models[i].id || expected.alias != actual.models[i].alias {
			return errors.New("CPA 未确认第三方模型别名")
		}
	}
	caps, _ := compatibleReasoningCaps(rules, actual.models)
	for _, model := range snapshot.Models {
		id := strings.ToLower(strings.TrimSpace(model.ID))
		if caps[id] != snapshot.ReasoningCaps[id] {
			return errors.New("CPA 未确认第三方模型思考强度上限")
		}
	}
	return nil
}

const compatibleEffortPath = "reasoning_effort"

func compatibleCapRules(model, cap string) []*yaml.Node {
	index := reasoningEffortIndex(cap)
	if index < 0 || cap == "ultra" {
		return nil
	}
	raw, _ := json.Marshal(cap)
	var rules []*yaml.Node
	for _, higher := range reasoningEffortOrder[index+1:] {
		selector := yamlMap("name", yamlScalar(model), "protocol", yamlScalar("openai"),
			"match", yamlSeq(yamlMap(compatibleEffortPath, yamlScalar(higher))))
		rules = append(rules, yamlMap("models", yamlSeq(selector), "params", yamlMap(compatibleEffortPath, yamlScalar(string(raw)))))
	}
	return rules
}

// Recognize complete generated groups, including after CPA rewrites comments.
func compatibleReasoningCaps(rules *yaml.Node, models []compatibleModel) (map[string]string, map[int]bool) {
	caps := map[string]string{}
	indexes := map[int]bool{}
	if rules == nil {
		return caps, indexes
	}
	known := map[string]string{}
	for _, model := range models {
		known[strings.ToLower(model.id)] = model.id
	}
	for index, rule := range rules.Content {
		name, cap, err := managedReasoningRule(rule)
		if err != nil || known[name] == "" {
			continue
		}
		expected := compatibleCapRules(known[name], cap)
		end := index + len(expected)
		if len(expected) == 0 || end > len(rules.Content) {
			continue
		}
		matches := true
		for i, want := range expected {
			if !sameReasoningRule(rules.Content[index+i], want) {
				matches = false
				break
			}
		}
		if matches {
			caps[name] = cap
			for i := index; i < end; i++ {
				indexes[i] = true
			}
		}
	}
	return caps, indexes
}

func updateCompatibleReasoning(doc, rules *yaml.Node, snapshot OAuthChannelPreset, providers map[string]compatibleProvider, groups map[string][]compatibleModel) error {
	p := providers[snapshot.Channel]
	_, indexes := compatibleReasoningCaps(rules, p.models)
	if len(indexes) == 0 && len(snapshot.ReasoningCaps) == 0 {
		return nil
	}
	if err := ensureReasoningRuleSlot(doc); err != nil {
		return err
	}
	_, rules, err := reasoningOverrideRaw(doc)
	if err != nil {
		return err
	}
	kept := make([]*yaml.Node, 0, len(rules.Content))
	for i, rule := range rules.Content {
		if !indexes[i] {
			kept = append(kept, rule)
		}
	}
	for _, model := range snapshot.Models {
		id := strings.ToLower(strings.TrimSpace(model.ID))
		cap := snapshot.ReasoningCaps[id]
		if cap == "" {
			continue
		}
		if !compatibleCapIsolated(providers, snapshot.Channel, id) {
			return errors.New("同名第三方模型无法单独限制思考强度")
		}
		kept = append(kept, compatibleCapRules(groups[id][0].id, cap)...)
	}
	rules.Content = kept
	return nil
}
