package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"cliproxy-portal/internal/agy"
)

func (s *Store) migrateAGYObservations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agy_observations (
 id TEXT NOT NULL,key_hash TEXT NOT NULL,request_id TEXT NOT NULL,at_ms INTEGER NOT NULL,payload TEXT NOT NULL,
 PRIMARY KEY(key_hash,id));
 CREATE INDEX IF NOT EXISTS idx_agy_observation_request ON agy_observations(key_hash,request_id,at_ms);
 CREATE INDEX IF NOT EXISTS idx_agy_observation_time ON agy_observations(at_ms);
 CREATE TABLE IF NOT EXISTS agy_summary_usage (
 id TEXT NOT NULL,key_hash TEXT NOT NULL,request_id TEXT NOT NULL,at_ms INTEGER NOT NULL,model TEXT NOT NULL,
 input_tokens INTEGER NOT NULL,output_tokens INTEGER NOT NULL,cached_tokens INTEGER NOT NULL,reasoning_tokens INTEGER NOT NULL,total_tokens INTEGER NOT NULL,usage_known INTEGER NOT NULL,
 PRIMARY KEY(key_hash,id));
 CREATE INDEX IF NOT EXISTS idx_agy_summary_key_time ON agy_summary_usage(key_hash,at_ms);`)
	return err
}

// The ledger deduplicates an actual summary prompt independently of HTTP
// attempts. Receipt retention cannot erase its billing evidence.
func (s *Store) SaveAGYObservation(ctx context.Context, v agy.Observation) error {
	_, key, ok := strings.Cut(v.Owner, ":")
	if !ok || key == "" || v.ID == "" || v.RequestID == "" || v.At <= 0 {
		return errors.New("invalid AGY observation identity")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agy_observations(id,key_hash,request_id,at_ms,payload) VALUES(?,?,?,?,?)`, v.ID, key, v.RequestID, v.At, string(data)); err != nil {
		return err
	}
	if v.Kind == "summary_usage" {
		u := v.Usage
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agy_summary_usage(id,key_hash,request_id,at_ms,model,input_tokens,output_tokens,cached_tokens,reasoning_tokens,total_tokens,usage_known) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.ID, key, v.RequestID, v.At, v.Model, u.Input, u.Output, u.Cache, u.Reasoning, u.Total, u.Known && v.State == "completed"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AGYObservations(ctx context.Context, requestID, key string) ([]agy.Observation, error) {
	if requestID == "" || key == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM agy_observations WHERE key_hash=? AND request_id=? ORDER BY at_ms,rowid LIMIT 2048`, key, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agy.Observation
	for rows.Next() {
		var data string
		var v agy.Observation
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) PruneAGYObservations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agy_observations WHERE at_ms<?`, time.Now().Add(-7*24*time.Hour).UnixMilli())
	return err
}
