package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var upstreamLayoutScope = regexp.MustCompile(`^(presets|aliases:[a-z][a-z0-9_-]{0,63})$`)

func ValidateUpstreamCardLayout(scope string, order []string) error {
	if !upstreamLayoutScope.MatchString(scope) || len(order) > 256 {
		return fmt.Errorf("卡片布局范围或数量无效")
	}
	seen := make(map[string]bool, len(order))
	for _, id := range order {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 128 || strings.ContainsAny(id, "\x00\r\n") || seen[id] {
			return fmt.Errorf("卡片标识无效或重复")
		}
		seen[id] = true
	}
	return nil
}

// Layouts are shared display preferences, separate from OAuth/preset configuration.
func (s *Store) UpstreamCardOrder(ctx context.Context, scope string) ([]string, error) {
	if err := ValidateUpstreamCardLayout(scope, nil); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, "upstream_card_layout:"+scope).Scan(&payload)
	if errors.Is(err, ErrNotFound) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var order []string
	if err := json.Unmarshal([]byte(payload), &order); err != nil {
		return nil, err
	}
	if err := ValidateUpstreamCardLayout(scope, order); err != nil {
		return nil, err
	}
	if order == nil {
		order = []string{}
	}
	return order, nil
}

func (s *Store) SetUpstreamCardOrder(ctx context.Context, scope string, order []string) error {
	if err := ValidateUpstreamCardLayout(scope, order); err != nil {
		return err
	}
	if order == nil {
		order = []string{}
	}
	payload, err := json.Marshal(order)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "upstream_card_layout:"+scope, string(payload))
	return err
}
