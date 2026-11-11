package store

import (
	"cliproxy-portal/internal/resourcecontrol"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const resourcePolicyKey = "resource_protection_v1"

// Normalize stored settings at startup, preserving the memory threshold and
// revision. Retired fields are omitted; normal reads never perform migrations.
func (s *Store) migrateResourcePolicyUnits(ctx context.Context) error {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", resourcePolicyKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var units struct {
		MemoryMiB *float64 `json:"memory_mib"`
		MemoryMB  *float64 `json:"memory_mb"`
	}
	if err := json.Unmarshal([]byte(raw), &units); err != nil {
		return err
	}
	if units.MemoryMiB != nil && units.MemoryMB != nil {
		return errors.New("资源保护设置的存储单位存在冲突")
	}
	p := resourcecontrol.DefaultPolicy()
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return err
	}
	if units.MemoryMiB != nil {
		p.MemoryMB = *units.MemoryMiB * float64(resourcecontrol.MiB) / 1_000_000
	}
	if err := p.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if string(b) == raw {
		return nil
	}
	_, err = s.db.ExecContext(ctx, "UPDATE settings SET value=? WHERE key=? AND value=?", string(b), resourcePolicyKey, raw)
	return err
}

func (s *Store) ResourcePolicy(ctx context.Context) (resourcecontrol.Policy, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", resourcePolicyKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return resourcecontrol.DefaultPolicy(), nil
	}
	if err != nil {
		return resourcecontrol.Policy{}, err
	}
	p := resourcecontrol.DefaultPolicy()
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
