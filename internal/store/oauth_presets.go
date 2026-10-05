package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const MaxOAuthPresets = 20

func (s *Store) SaveOAuthPreset(ctx context.Context, preset OAuthPreset) (OAuthPreset, error) {
	preset.Name = strings.TrimSpace(preset.Name)
	if preset.ID == "" || preset.Name == "" || preset.Payload == "" {
		return OAuthPreset{}, errors.New("invalid OAuth preset")
	}
	var format struct {
		Version  int `json:"version"`
		Channels []struct {
			Channel string `json:"channel"`
		} `json:"channels"`
	}
	if json.Unmarshal([]byte(preset.Payload), &format) != nil || format.Version != 3 || len(format.Channels) == 0 {
		return OAuthPreset{}, errors.New("invalid OAuth preset format")
	}
	seen := map[string]bool{}
	for _, entry := range format.Channels {
		channel := strings.ToLower(strings.TrimSpace(entry.Channel))
		if channel == "" || seen[channel] {
			return OAuthPreset{}, errors.New("invalid OAuth preset channels")
		}
		seen[channel] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthPreset{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM oauth_presets WHERE name=? COLLATE NOCASE`, preset.Name).Scan(&existingID)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE oauth_presets SET name=?,payload=?,updated_by=?,updated_at_ms=? WHERE id=?`, preset.Name, preset.Payload, preset.UpdatedBy, timeMS(now), existingID)
		if err != nil {
			return OAuthPreset{}, err
		}
		if err := tx.Commit(); err != nil {
			return OAuthPreset{}, err
		}
		return s.OAuthPreset(ctx, existingID)
	}
	if !errors.Is(err, ErrNotFound) {
		return OAuthPreset{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_presets`).Scan(&count); err != nil {
		return OAuthPreset{}, err
	}
	if count >= MaxOAuthPresets {
		return OAuthPreset{}, errors.New("OAuth preset limit reached")
	}
	if preset.CreatedAt.IsZero() {
		preset.CreatedAt = now
	}
	preset.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `INSERT INTO oauth_presets(id,channel,name,payload,updated_by,created_at_ms,updated_at_ms) VALUES(?,'all',?,?,?,?,?)`, preset.ID, preset.Name, preset.Payload, preset.UpdatedBy, timeMS(preset.CreatedAt), timeMS(preset.UpdatedAt))
	if err != nil {
		return OAuthPreset{}, err
	}
	return preset, tx.Commit()
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

// Upgrade all stored presets once to the global version-3 envelope. Same-name
// channel presets are combined without inventing missing channel settings.
// Unsupported/malformed payloads abort the transaction instead of losing data.
func (s *Store) migrateOAuthPresetPayloads(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,channel,name,payload,updated_at_ms FROM oauth_presets ORDER BY updated_at_ms DESC,id`)
	if err != nil {
		return err
	}
	type group struct {
		id, name string
		channels map[string]json.RawMessage
		fields   map[string]json.RawMessage
		deleted  []string
	}
	groups := map[string]*group{}
	for rows.Next() {
		var id, channel, name, payload string
		var updated int64
		if err := rows.Scan(&id, &channel, &name, &payload, &updated); err != nil {
			rows.Close()
			return err
		}
		var fields map[string]json.RawMessage
		invalid := func() error { rows.Close(); return fmt.Errorf("调用预设 %s 内容无效，迁移已取消", name) }
		if json.Unmarshal([]byte(payload), &fields) != nil || fields == nil {
			return invalid()
		}
		var version int
		if json.Unmarshal(fields["version"], &version) != nil {
			return invalid()
		}
		var entries []json.RawMessage
		switch version {
		case 1, 2:
			var storedChannel string
			if raw, ok := fields["channel"]; ok && json.Unmarshal(raw, &storedChannel) != nil {
				return invalid()
			}
			if storedChannel == "" && version == 1 && channel == "codex" {
				storedChannel = channel
			}
			if !strings.EqualFold(strings.TrimSpace(storedChannel), channel) || storedChannel == "" {
				return invalid()
			}
			delete(fields, "version")
			fields["channel"], _ = json.Marshal(channel)
			entry, err := json.Marshal(fields)
			if err != nil {
				return invalid()
			}
			entries = []json.RawMessage{entry}
			fields = map[string]json.RawMessage{}
		case 3:
			if json.Unmarshal(fields["channels"], &entries) != nil || len(entries) == 0 {
				return invalid()
			}
		default:
			return invalid()
		}
		// Match SQLite COLLATE NOCASE (ASCII folding), not Unicode case folding.
		key := strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' {
				return r + 'a' - 'A'
			}
			return r
		}, name)
		g := groups[key]
		if g == nil {
			g = &group{id: id, name: name, channels: map[string]json.RawMessage{}, fields: fields}
			groups[key] = g
		} else {
			g.deleted = append(g.deleted, id)
		}
		local := map[string]bool{}
		for _, entry := range entries {
			var channelFields map[string]json.RawMessage
			var value string
			if json.Unmarshal(entry, &channelFields) != nil || json.Unmarshal(channelFields["channel"], &value) != nil {
				return invalid()
			}
			value = strings.ToLower(strings.TrimSpace(value))
			if value == "" || local[value] {
				return invalid()
			}
			local[value] = true
			// Newest saved data wins if duplicate legacy entries exist.
			if _, exists := g.channels[value]; !exists {
				g.channels[value] = entry
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Remove redundant rows before assigning the shared namespace to avoid the
	// old UNIQUE(channel,name) constraint during a same-name merge.
	for _, g := range groups {
		for _, id := range g.deleted {
			if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_presets WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	for _, g := range groups {
		keys := make([]string, 0, len(g.channels))
		for channel := range g.channels {
			keys = append(keys, channel)
		}
		sort.Strings(keys)
		entries := make([]json.RawMessage, 0, len(keys))
		for _, channel := range keys {
			entries = append(entries, g.channels[channel])
		}
		g.fields["version"] = json.RawMessage("3")
		g.fields["channels"], _ = json.Marshal(entries)
		data, err := json.Marshal(g.fields)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_presets SET channel='all',payload=? WHERE id=?`, string(data), g.id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_presets_global_name ON oauth_presets(name COLLATE NOCASE)`); err != nil {
		return err
	}
	return tx.Commit()
}
