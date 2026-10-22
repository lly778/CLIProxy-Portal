package store

import (
	"cliproxy-portal/internal/resourcecontrol"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Keep Linux whole-machine CPU + memory separate from earlier suite-memory
// series and legacy host data whose memory accounting may differ.
const resourceHistoryKey = "resource_history_linux_host_v1"

func (s *Store) ResourceHistory(ctx context.Context) ([]resourcecontrol.HistoryPoint, error) {
	return s.ResourceHistoryForScope(ctx, "")
}

func historyKey(scope string) string {
	if scope == "esxi" {
		return "resource_history_esxi_host_v1"
	}
	return resourceHistoryKey
}

func (s *Store) ResourceHistoryForScope(ctx context.Context, scope string) ([]resourcecontrol.HistoryPoint, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", historyKey(scope)).Scan(&raw)
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
	return s.SaveResourceHistoryForScope(ctx, points, "")
}

func (s *Store) SaveResourceHistoryForScope(ctx context.Context, points []resourcecontrol.HistoryPoint, scope string) error {
	if len(points) > resourcecontrol.HistoryCapacity {
		return errors.New("resource history exceeds retention capacity")
	}
	data, err := json.Marshal(points)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", historyKey(scope), string(data))
	return err
}
