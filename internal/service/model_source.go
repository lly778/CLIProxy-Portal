package service

import (
	"context"
	"errors"

	"cliproxy-portal/internal/cpamp"
)

// One model metadata source serves pickers, upstream management and visibility
// rules. Contexts used for mutations/cooldown checks deliberately bypass caching.
func (k *Keys) upstreamAuthFiles(ctx context.Context) ([]cpamp.AuthFile, error) {
	return upstreamView(ctx, k, "auth-files", func(ctx context.Context) ([]cpamp.AuthFile, error) {
		reader, ok := k.CPAMP.(interface {
			ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
		})
		if !ok {
			return nil, errors.New("CPAMP 客户端不支持读取上游账号")
		}
		return reader.ListAuthFiles(ctx)
	}, func(value []cpamp.AuthFile) []cpamp.AuthFile {
		value = append([]cpamp.AuthFile(nil), value...)
		for index := range value {
			value[index].Cooldowns = append([]cpamp.AuthCooldown(nil), value[index].Cooldowns...)
		}
		return value
	})
}

func (k *Keys) upstreamModelDefinitions(ctx context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	return upstreamView(ctx, k, "definitions:"+channel, func(ctx context.Context) ([]cpamp.OAuthModelDefinition, error) {
		reader, ok := k.CPAMP.(interface {
			ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error)
		})
		if !ok {
			return nil, errors.New("CPAMP 客户端不支持读取模型目录")
		}
		return reader.ListOAuthModelDefinitions(ctx, channel)
	}, func(value []cpamp.OAuthModelDefinition) []cpamp.OAuthModelDefinition {
		value = append([]cpamp.OAuthModelDefinition(nil), value...)
		for index := range value {
			value[index].ThinkingLevels = append([]string(nil), value[index].ThinkingLevels...)
		}
		return value
	})
}

func (k *Keys) upstreamRegisteredModels(ctx context.Context, file cpamp.AuthFile) ([]cpamp.Model, error) {
	return upstreamView(ctx, k, "registered:"+upstreamAccountID(file), func(ctx context.Context) ([]cpamp.Model, error) {
		reader, ok := k.CPAMP.(interface {
			ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error)
		})
		if !ok {
			return nil, errors.New("CPAMP 客户端不支持读取注册模型")
		}
		return reader.ListAuthFileModels(ctx, file)
	}, func(value []cpamp.Model) []cpamp.Model { return append([]cpamp.Model(nil), value...) })
}

func (k *Keys) modelsWithAPIKey(ctx context.Context, key string) (cpamp.ModelListResponse, error) {
	reader, ok := k.CPAMP.(interface {
		ListModelsWithAPIKey(context.Context, string) (cpamp.ModelListResponse, error)
	})
	if !ok {
		return cpamp.ModelListResponse{}, errors.New("CPAMP 客户端不支持模型查询")
	}
	// Reading this endpoint also verifies the caller's actual key. Never reuse
	// one account's runtime response for another account, or cache a key test.
	return reader.ListModelsWithAPIKey(ctx, key)
}
