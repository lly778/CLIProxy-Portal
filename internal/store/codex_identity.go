package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

const codexIdentitySettingPrefix = "codex_identity_compatibility:"

var identityChannelPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Channel preferences live in settings so existing database backups include them.
func (s *Store) EnabledCodexIdentityChannels(ctx context.Context) (map[string]bool, error) {
	if values, ok := ctx.Value(upstreamPageSettingsKey{}).(*upstreamPageSettings); ok {
		if values.err != nil {
			return nil, values.err
		}
		values.identityOnce.Do(func() { values.identity, values.identityErr = enabledIdentityChannels(values.values) })
		channels := make(map[string]bool, len(values.identity))
		for channel, enabled := range values.identity {
			channels[channel] = enabled
		}
		return channels, values.identityErr
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key LIKE 'codex_identity_compatibility:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		settings[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return enabledIdentityChannels(settings)
}

func enabledIdentityChannels(settings map[string]string) (map[string]bool, error) {
	channels := make(map[string]bool)
	for key, value := range settings {
		channel := strings.TrimPrefix(key, codexIdentitySettingPrefix)
		if !strings.HasPrefix(key, codexIdentitySettingPrefix) || !identityChannelPattern.MatchString(channel) || channel == "codex" {
			continue
		}
		if value != "true" && value != "false" {
			return nil, errors.New("Codex 指令兼容设置无效")
		}
		if value == "true" {
			channels[channel] = true
		}
	}
	return channels, nil
}

func (s *Store) SetCodexIdentityCompatibility(ctx context.Context, channel string, enabled bool) error {
	if !identityChannelPattern.MatchString(channel) || channel == "codex" {
		return errors.New("仅非 Codex 渠道支持指令兼容设置")
	}
	value := "false"
	if enabled {
		value = "true"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, codexIdentitySettingPrefix+channel, value)
	return err
}
