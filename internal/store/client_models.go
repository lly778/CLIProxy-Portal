package store

import (
	"context"
	"encoding/json"
	"errors"

	"cliproxy-portal/internal/domain"
)

const clientModelsKey = "client_models:v1"

var ErrClientModelsConflict = errors.New("客户端模型配置已被其他管理员更新，请刷新页面后重试")

func (s *Store) ClientModels(ctx context.Context) (domain.ClientModels, error) {
	payload, err := s.upstreamSetting(ctx, clientModelsKey)
	if errors.Is(err, ErrNotFound) {
		return domain.ClientModels{Version: domain.ClientModelsVersion, Models: []domain.ClientModel{}}, nil
	}
	if err != nil {
		return domain.ClientModels{}, err
	}
	var config domain.ClientModels
	if err = json.Unmarshal([]byte(payload), &config); err != nil {
		return config, err
	}
	return config, config.Validate()
}

// A compare-and-swap protects a shared publication from stale admin forms.
func (s *Store) SaveClientModels(ctx context.Context, config domain.ClientModels, expected int64) error {
	config.Version = domain.ClientModelsVersion
	// The current deployment's API URL is supplied at fetch time, not restored
	// from an old publication when this database moves to another server.
	config.APIBaseURL = ""
	config.Models = append([]domain.ClientModel{}, config.Models...)
	for index := range config.Models {
		config.Models[index].ReasoningLevels = nil
		config.Models[index].SupportedReasoningLevels = nil
	}
	config.Revision = expected + 1
	if expected < 0 || config.Validate() != nil {
		return errors.New("客户端模型配置无效")
	}
	payload, err := json.Marshal(config)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, clientModelsKey).Scan(&previous)
	if errors.Is(err, ErrNotFound) {
		if expected != 0 {
			return ErrClientModelsConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)`, clientModelsKey, string(payload))
	} else if err == nil {
		var current domain.ClientModels
		if err = json.Unmarshal([]byte(previous), &current); err != nil {
			return err
		}
		if err = current.Validate(); err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrClientModelsConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, string(payload), clientModelsKey)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
