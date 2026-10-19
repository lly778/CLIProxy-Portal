package store

import (
	"cliproxy-portal/internal/resourcecontrol"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const resourceHistoryKey = "resource_history_v1"

func (s *Store) ResourceHistory(ctx context.Context) ([]resourcecontrol.HistoryPoint, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", resourceHistoryKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var points []resourcecontrol.HistoryPoint
	if err := json.Unmarshal([]byte(raw), &points); err != nil {
		return nil, err
	}
	return points, nil
}

func (s *Store) SaveResourceHistory(ctx context.Context, points []resourcecontrol.HistoryPoint) error {
	if len(points) > resourcecontrol.HistoryCapacity {
		return errors.New("resource history exceeds retention capacity")
	}
	data, err := json.Marshal(points)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", resourceHistoryKey, string(data))
	return err
}
