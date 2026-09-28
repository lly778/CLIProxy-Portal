package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

const MaxOAuthPresets = 20

func (s *Store) SaveOAuthPreset(ctx context.Context, preset OAuthPreset) (OAuthPreset, error) {
	preset.Name = strings.TrimSpace(preset.Name)
	if preset.ID == "" || preset.Name == "" || preset.Payload == "" {
		return OAuthPreset{}, errors.New("invalid OAuth preset")
	}
	now := time.Now().UTC()
	var existingID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM oauth_presets WHERE name=? COLLATE NOCASE`, preset.Name).Scan(&existingID)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE oauth_presets SET name=?,payload=?,updated_by=?,updated_at_ms=? WHERE id=?`, preset.Name, preset.Payload, preset.UpdatedBy, timeMS(now), existingID)
		if err != nil {
			return OAuthPreset{}, err
		}
		return s.OAuthPreset(ctx, existingID)
	}
	if !errors.Is(err, ErrNotFound) {
		return OAuthPreset{}, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_presets`).Scan(&count); err != nil {
		return OAuthPreset{}, err
	}
	if count >= MaxOAuthPresets {
		return OAuthPreset{}, errors.New("OAuth preset limit reached")
	}
	if preset.CreatedAt.IsZero() {
		preset.CreatedAt = now
	}
	preset.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `INSERT INTO oauth_presets(id,name,payload,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?)`, preset.ID, preset.Name, preset.Payload, preset.UpdatedBy, timeMS(preset.CreatedAt), timeMS(preset.UpdatedAt))
	if err != nil {
		return OAuthPreset{}, err
	}
	return preset, nil
}

func (s *Store) OAuthPreset(ctx context.Context, id string) (OAuthPreset, error) {
	var preset OAuthPreset
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,payload,updated_by,created_at_ms,updated_at_ms FROM oauth_presets WHERE id=?`, id).Scan(&preset.ID, &preset.Name, &preset.Payload, &preset.UpdatedBy, &created, &updated)
	preset.CreatedAt, preset.UpdatedAt = fromMS(created), fromMS(updated)
	return preset, err
}

func (s *Store) ListOAuthPresets(ctx context.Context) ([]OAuthPreset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,payload,updated_by,created_at_ms,updated_at_ms FROM oauth_presets ORDER BY updated_at_ms DESC,name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var presets []OAuthPreset
	for rows.Next() {
		var preset OAuthPreset
		var created, updated int64
		if err := rows.Scan(&preset.ID, &preset.Name, &preset.Payload, &preset.UpdatedBy, &created, &updated); err != nil {
			return nil, err
		}
		preset.CreatedAt, preset.UpdatedAt = fromMS(created), fromMS(updated)
		presets = append(presets, preset)
	}
	return presets, rows.Err()
}

func (s *Store) DeleteOAuthPreset(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM oauth_presets WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrNotFound
	}
	return nil
}
