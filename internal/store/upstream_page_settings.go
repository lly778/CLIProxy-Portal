package store

import (
	"context"
	"sync"
)

type upstreamPageSettingsKey struct{}
type upstreamPageSettings struct {
	values       map[string]string
	err          error
	identityOnce sync.Once
	identity     map[string]bool
	identityErr  error
}

// WithUpstreamPageSettings batches this page's small settings into one SQL read.
// It is request-local, so another administrator's saved settings remain visible immediately.
func (s *Store) WithUpstreamPageSettings(ctx context.Context) context.Context {
	value := &upstreamPageSettings{values: make(map[string]string)}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN (?,?,?) OR key LIKE 'codex_identity_compatibility:%'`, clientModelsKey, "upstream_card_layout:presets", agySettingsKey)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key, payload string
			if err = rows.Scan(&key, &payload); err != nil {
				break
			}
			value.values[key] = payload
		}
		if err == nil {
			err = rows.Err()
		}
	}
	value.err = err
	return context.WithValue(ctx, upstreamPageSettingsKey{}, value)
}

func (s *Store) upstreamSetting(ctx context.Context, key string) (string, error) {
	if values, ok := ctx.Value(upstreamPageSettingsKey{}).(*upstreamPageSettings); ok {
		if values.err != nil {
			return "", values.err
		}
		value, found := values.values[key]
		if !found {
			return "", ErrNotFound
		}
		return value, nil
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	return value, err
}
