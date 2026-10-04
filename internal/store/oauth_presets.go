package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const MaxOAuthPresets = 20

func (s *Store) SaveOAuthPreset(ctx context.Context, preset OAuthPreset) (OAuthPreset, error) {
	preset.Name = strings.TrimSpace(preset.Name)
	preset.Channel = presetChannel(preset.Channel)
	if preset.ID == "" || preset.Name == "" || preset.Payload == "" {
		return OAuthPreset{}, errors.New("invalid OAuth preset")
	}
	now := time.Now().UTC()
	var existingID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM oauth_presets WHERE channel=? AND name=? COLLATE NOCASE`, preset.Channel, preset.Name).Scan(&existingID)
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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_presets WHERE channel=?`, preset.Channel).Scan(&count); err != nil {
		return OAuthPreset{}, err
	}
	if count >= MaxOAuthPresets {
		return OAuthPreset{}, errors.New("OAuth preset limit reached")
	}
	if preset.CreatedAt.IsZero() {
		preset.CreatedAt = now
	}
	preset.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `INSERT INTO oauth_presets(id,channel,name,payload,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?)`, preset.ID, preset.Channel, preset.Name, preset.Payload, preset.UpdatedBy, timeMS(preset.CreatedAt), timeMS(preset.UpdatedAt))
	if err != nil {
		return OAuthPreset{}, err
	}
	return preset, nil
}

func (s *Store) OAuthPreset(ctx context.Context, id string) (OAuthPreset, error) {
	var preset OAuthPreset
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,channel,name,payload,updated_by,created_at_ms,updated_at_ms FROM oauth_presets WHERE id=?`, id).Scan(&preset.ID, &preset.Channel, &preset.Name, &preset.Payload, &preset.UpdatedBy, &created, &updated)
	preset.CreatedAt, preset.UpdatedAt = fromMS(created), fromMS(updated)
	return preset, err
}

func (s *Store) ListOAuthPresets(ctx context.Context, channels ...string) ([]OAuthPreset, error) {
	channel := "codex"
	if len(channels) > 0 {
		channel = presetChannel(channels[0])
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,channel,name,payload,updated_by,created_at_ms,updated_at_ms FROM oauth_presets WHERE channel=? ORDER BY updated_at_ms DESC,name COLLATE NOCASE`, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var presets []OAuthPreset
	for rows.Next() {
		var preset OAuthPreset
		var created, updated int64
		if err := rows.Scan(&preset.ID, &preset.Channel, &preset.Name, &preset.Payload, &preset.UpdatedBy, &created, &updated); err != nil {
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

func presetChannel(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return "codex"
	}
	return channel
}

// Preserve legacy presets as Codex and replace the old global name uniqueness
// with channel/name uniqueness in one transaction. Reopening is idempotent.
func (s *Store) migrateOAuthPresetChannels(ctx context.Context) error {
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('oauth_presets') WHERE name='channel'`).Scan(&present); err != nil {
		return err
	}
	if present != 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE oauth_presets_channels (
    id TEXT PRIMARY KEY,
    channel TEXT NOT NULL DEFAULT 'codex',
    name TEXT NOT NULL COLLATE NOCASE,
    payload TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    UNIQUE(channel,name)
  )`,
		`INSERT INTO oauth_presets_channels(id,channel,name,payload,updated_by,created_at_ms,updated_at_ms)
   SELECT id,'codex',name,payload,updated_by,created_at_ms,updated_at_ms FROM oauth_presets`,
		`DROP TABLE oauth_presets`,
		`ALTER TABLE oauth_presets_channels RENAME TO oauth_presets`,
		`CREATE INDEX idx_oauth_presets_updated ON oauth_presets(updated_at_ms DESC)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Upgrade version-1 preset data to the explicit channel-scoped version-2 format.
// Retain all existing fields and metadata; unknown or malformed formats are not
// silently reinterpreted. Only the migration understands the old representation.
func (s *Store) migrateOAuthPresetPayloads(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,channel,payload FROM oauth_presets`)
	if err != nil {
		return err
	}
	type update struct{ id, payload string }
	var updates []update
	for rows.Next() {
		var id, channel, payload string
		if err := rows.Scan(&id, &channel, &payload); err != nil {
			rows.Close()
			return err
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &fields) != nil || fields == nil {
			continue
		}
		var version int
		if json.Unmarshal(fields["version"], &version) != nil || version != 1 {
			continue
		}
		var storedChannel string
		if raw, ok := fields["channel"]; ok && json.Unmarshal(raw, &storedChannel) != nil {
			continue
		}
		// A pre-channel preset may only belong to Codex. Never relabel a
		// mismatched explicit channel or assign incomplete data elsewhere.
		if storedChannel == "" && channel != "codex" {
			continue
		}
		if storedChannel != "" && !strings.EqualFold(strings.TrimSpace(storedChannel), channel) {
			continue
		}
		fields["channel"], _ = json.Marshal(channel)
		fields["version"] = json.RawMessage("2")
		data, err := json.Marshal(fields)
		if err != nil {
			rows.Close()
			return err
		}
		updates = append(updates, update{id: id, payload: string(data)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_presets SET payload=? WHERE id=?`, item.payload, item.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
