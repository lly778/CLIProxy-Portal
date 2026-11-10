package store

import (
	"context"
	"encoding/json"
	"errors"

	"cliproxy-portal/internal/agy"
)

const agySettingsKey = "agy:settings:v1"

func (s *Store) AGYSettings(ctx context.Context) (agy.Settings, error) {
	raw, err := s.upstreamSetting(ctx, agySettingsKey)
	if errors.Is(err, ErrNotFound) {
		return agy.Settings{Enabled: true, Models: []agy.Model{}, Rules: []agy.ModelRule{}}, nil
	}
	if err != nil {
		return agy.Settings{}, err
	}
	var v agy.Settings
	if err = json.Unmarshal([]byte(raw), &v); err != nil {
		return v, err
	}
	// Legacy trial switches remain readable in backups, but access is now
	// governed by normal portal approval and the shared per-model rules.
	v.Enabled, v.AdminOnly = true, false
	return v, v.Validate()
}
func (s *Store) SaveAGYSettings(ctx context.Context, v agy.Settings, expected int64) error {
	v.Enabled, v.AdminOnly = true, false
	if expected < 0 {
		return errors.New("AGY 配置版本无效")
	}
	v.Revision = expected + 1
	if e := v.Validate(); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var raw string
	e = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, agySettingsKey).Scan(&raw)
	if errors.Is(e, ErrNotFound) {
		if expected != 0 {
			return ErrClientModelsConflict
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)`, agySettingsKey, string(b))
	} else if e == nil {
		var old agy.Settings
		if json.Unmarshal([]byte(raw), &old) != nil || old.Revision != expected {
			return ErrClientModelsConflict
		}
		_, e = tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, string(b), agySettingsKey)
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
