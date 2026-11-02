package service

import (
	"context"
	"errors"
	"strings"

	"cliproxy-portal/internal/domain"
)

var ErrClientModelsUnavailable = errors.New("管理员尚未设置客户端模型配置")

// PublishedClientModels contains only the administrator's public client defaults.
// Actual API requests still require their own API key and upstream permissions.
func (k *Keys) PublishedClientModels(ctx context.Context) (domain.ClientModels, error) {
	config, err := k.Store.ClientModels(ctx)
	if err != nil {
		return domain.ClientModels{}, err
	}
	if len(config.Models) == 0 {
		return domain.ClientModels{}, ErrClientModelsUnavailable
	}
	config.Enabled = true
	config.Version = domain.ClientModelsVersion
	// Reuse the bounded, coalesced static catalog. A temporary management failure
	// must not prevent fetching saved defaults or preserving native client data.
	declared := map[string][]string{}
	if catalog, catalogErr := k.ClientModelCatalog(ctx); catalogErr == nil {
		for _, model := range catalog {
			declared[strings.ToLower(model.ID)] = model.SupportedReasoningLevels
		}
	}
	for index := range config.Models {
		config.Models[index].ReasoningLevels = nil
		config.Models[index].SupportedReasoningLevels = append([]string(nil), declared[strings.ToLower(config.Models[index].ID)]...)
	}
	return config, config.Validate()
}
