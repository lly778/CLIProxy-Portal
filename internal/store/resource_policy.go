package store

import (
	"cliproxy-portal/internal/resourcecontrol"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const resourcePolicyKey = "resource_protection_v1"

func (s *Store) ResourcePolicy(ctx context.Context) (resourcecontrol.Policy, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", resourcePolicyKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return resourcecontrol.DefaultPolicy(), nil
	}
	if err != nil {
		return resourcecontrol.Policy{}, err
	}
	var p resourcecontrol.Policy
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}
func (s *Store) SaveResourcePolicy(ctx context.Context, p resourcecontrol.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", resourcePolicyKey).Scan(&raw)
	current := resourcecontrol.DefaultPolicy()
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &current); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if p.Revision != current.Revision {
		return errors.New("设置已被其他管理员更新，请刷新后重试")
	}
	p.Revision++
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", resourcePolicyKey, string(b))
	if err != nil {
		return err
	}
	return tx.Commit()
}
