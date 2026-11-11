package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"gopkg.in/yaml.v3"
)

// ClientCatalogModel is sanitized static metadata, with no upstream credentials.
type ClientCatalogModel struct {
	ID                       string
	SupportedReasoningLevels []string
}

func cloneClientCatalog(models []ClientCatalogModel) []ClientCatalogModel {
	cloned := append([]ClientCatalogModel{}, models...)
	for i := range cloned {
		cloned[i].SupportedReasoningLevels = append([]string(nil), models[i].SupportedReasoningLevels...)
	}
	return cloned
}

// Merge static definitions, aliases and custom API models without filtering
// exclusions, account state, quota or cooldown. Keep names and declared levels.
func (k *Keys) readClientModelCatalog(ctx context.Context) ([]ClientCatalogModel, error) {
	ctx = cpamp.WithUpstreamReadCache(ctx)
	channels, err := k.oauthChannels(ctx, true)
	if err != nil {
		return nil, err
	}
	present, err := k.OAuthAccountChannels(ctx)
	if err != nil {
		return nil, err
	}
	_, ok := k.CPAMP.(interface {
		ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error)
		ListOAuthModelAliases(context.Context, string) ([]cpamp.OAuthModelAlias, error)
	})
	if !ok {
		return nil, errors.New("管理服务暂不支持完整模型目录")
	}
	seen := map[string]int{}
	models := []ClientCatalogModel{}
	aliasSources := map[string]string{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		key := strings.ToLower(id)
		if _, exists := seen[key]; domain.ValidClientModelID(id) && !exists {
			seen[key] = len(models)
			models = append(models, ClientCatalogModel{ID: id})
		}
	}
	setLevels := func(id string, levels []string) {
		index, exists := seen[strings.ToLower(strings.TrimSpace(id))]
		if !exists {
			return
		}
		all := map[string]bool{}
		for _, level := range append(append([]string{}, models[index].SupportedReasoningLevels...), levels...) {
			all[strings.ToLower(strings.TrimSpace(level))] = true
		}
		models[index].SupportedReasoningLevels = nil
		for _, level := range domain.ClientReasoningLevels() {
			if all[level] {
				models[index].SupportedReasoningLevels = append(models[index].SupportedReasoningLevels, level)
			}
		}
	}
	for _, channel := range channels {
		if present != nil && !present[channel] {
			continue
		}
		definitions, definitionErr := k.upstreamModelDefinitions(ctx, channel)
		if definitionErr != nil {
			var status *cpamp.HTTPError
			// Custom channels may define aliases without a built-in model catalog.
			if !errors.As(definitionErr, &status) || status.StatusCode != 404 {
				return nil, fmt.Errorf("读取模型目录失败：%w", definitionErr)
			}
		}
		for _, model := range definitions {
			add(model.ID)
			setLevels(model.ID, model.ThinkingLevels)
		}
		aliases, _, aliasErr := k.OAuthModelAliases(ctx, channel)
		if aliasErr != nil {
			return nil, aliasErr
		}
		for _, alias := range aliases {
			add(alias.Name)
			add(alias.Alias)
			aliasSources[strings.ToLower(alias.Alias)] = strings.ToLower(alias.Name)
		}
	}
	// Read registration metadata even for disabled/cooling credentials. Those
	// models may be absent from both the live client list and static definitions.
	if _, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
		ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error)
	}); ok {
		files, filesErr := k.upstreamAuthFiles(ctx)
		if filesErr != nil {
			return nil, filesErr
		}
		seenFiles := map[string]bool{}
		for _, file := range files {
			identity := upstreamAccountID(file)
			if seenFiles[identity] {
				continue
			}
			seenFiles[identity] = true
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			registered, readErr := k.upstreamRegisteredModels(ctx, file)
			if readErr != nil {
				continue
			}
			for _, model := range registered {
				add(model.ID)
			}
		}
	}
	if configReader, ok := k.CPAMP.(interface {
		GetConfigYAML(context.Context) ([]byte, error)
	}); ok {
		data, readErr := configReader.GetConfigYAML(ctx)
		if readErr != nil {
			return nil, readErr
		}
		ids, parseErr := clientConfiguredModels(data)
		if parseErr != nil {
			return nil, parseErr
		}
		for _, id := range ids {
			add(id)
		}
		for alias, source := range clientConfiguredModelAliases(data) {
			aliasSources[alias] = source
		}
		if metadataReader, ok := k.CPAMP.(interface {
			ListConfiguredModelReasoning(context.Context, []byte) []cpamp.ConfiguredModelReasoning
		}); ok {
			for _, model := range metadataReader.ListConfiguredModelReasoning(ctx, data) {
				add(model.ID)
				setLevels(model.ID, model.Levels)
			}
		}
	}
	// Dynamic registrations can supplement the catalog. Lack of a client key
	// must not prevent editing static definitions on a fresh installation.
	if live, liveErr := k.Models(ctx); liveErr == nil {
		for _, model := range live {
			add(model.ID)
		}
	}
	// Resolve chains, including configured prefixes; never infer from name alone.
	for range len(aliasSources) {
		changed := false
		for alias, source := range aliasSources {
			sourceIndex, sourceOK := seen[source]
			aliasIndex, aliasOK := seen[alias]
			if !sourceOK || !aliasOK {
				continue
			}
			before := len(models[aliasIndex].SupportedReasoningLevels)
			setLevels(alias, models[sourceIndex].SupportedReasoningLevels)
			changed = changed || len(models[aliasIndex].SupportedReasoningLevels) != before
		}
		if !changed {
			break
		}
	}
	additional, agyErr := k.agyCatalog(ctx)
	if agyErr != nil {
		return nil, agyErr
	}
	for _, m := range additional {
		add(m.ID)
		setLevels(m.ID, m.SupportedReasoningLevels)
	}
	sort.Slice(models, func(i, j int) bool { return ModelIDLess(models[i].ID, models[j].ID) })
	return models, nil
}

func clientConfiguredModelAliases(data []byte) map[string]string {
	// Read only model names and aliases from known sections; no credentials escape.
	type entry struct {
		Prefix string `yaml:"prefix"`
		Models []struct {
			Name  string `yaml:"name"`
			Alias string `yaml:"alias"`
		} `yaml:"models"`
	}
	var config map[string][]entry
	// The full config also contains scalar values, so decode the named sections only.
	var raw map[string]yaml.Node
	if yaml.Unmarshal(data, &raw) != nil {
		return nil
	}
	config = map[string][]entry{}
	for _, section := range []string{"openai-compatibility", "codex-api-key", "claude-api-key", "gemini-api-key", "vertex-api-key"} {
		if node, ok := raw[section]; ok {
			var entries []entry
			if node.Decode(&entries) == nil {
				config[section] = entries
			}
		}
	}
	result := map[string]string{}
	for _, entries := range config {
		for _, provider := range entries {
			for _, model := range provider.Models {
				name, alias := strings.ToLower(model.Name), strings.ToLower(model.Alias)
				if name == "" {
					continue
				}
				if alias != "" {
					result[alias] = name
				}
				if provider.Prefix != "" {
					result[strings.ToLower(provider.Prefix)+"/"+name] = name
					if alias != "" {
						result[strings.ToLower(provider.Prefix)+"/"+alias] = name
					}
				}
			}
		}
	}
	return result
}

func clientConfiguredModels(data []byte) ([]string, error) {
	type entry struct {
		Prefix string `yaml:"prefix"`
		Models []struct {
			Name  string `yaml:"name"`
			Alias string `yaml:"alias"`
		} `yaml:"models"`
	}
	var config struct {
		OpenAI []entry `yaml:"openai-compatibility"`
		Codex  []entry `yaml:"codex-api-key"`
		Claude []entry `yaml:"claude-api-key"`
		Gemini []entry `yaml:"gemini-api-key"`
		Vertex []entry `yaml:"vertex-api-key"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, errors.New("无法读取自定义模型目录")
	}
	ids := []string{}
	for _, group := range [][]entry{config.OpenAI, config.Codex, config.Claude, config.Gemini, config.Vertex} {
		for _, provider := range group {
			for _, model := range provider.Models {
				for _, id := range []string{model.Name, model.Alias} {
					if id != "" {
						ids = append(ids, id)
						if provider.Prefix != "" {
							ids = append(ids, provider.Prefix+"/"+id)
						}
					}
				}
			}
		}
	}
	return ids, nil
}
