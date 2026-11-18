package store

import (
	"context"
	"encoding/json"
	"errors"
)

const compatibleModelsPrefix = "compatible_models:"

// CompatibleModelCatalog stores model mappings removed from CPA's active list.
// It contains only model definitions, never provider credentials or endpoints.
func (s *Store) CompatibleModelCatalog(ctx context.Context, channel string) ([]string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", compatibleModelsPrefix+channel).Scan(&raw)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var models []string
	if err := json.Unmarshal([]byte(raw), &models); err != nil {
		return nil, errors.New("无法读取已停用的第三方模型")
	}
	return models, nil
}

func (s *Store) SaveCompatibleModelCatalog(ctx context.Context, channel string, models []string) error {
	if len(models) == 0 {
		_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key=?", compatibleModelsPrefix+channel)
		return err
	}
	data, err := json.Marshal(models)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", compatibleModelsPrefix+channel, data)
	return err
}
