package store

import (
	"cliproxy-portal/internal/domain"
	"context"
	"time"
)

type ResourceCounts struct {
	Rejected uint64
	Observed uint64
}

// ResourceRequestEvents shares the existing durable event storage and seven-day
// counters. Moving their presentation does not discard or duplicate old records.
func (s *Store) ResourceRequestEvents(ctx context.Context, from, to time.Time, userID string, limit int) ([]domain.AuditEvent, int64, error) {
	where := ` WHERE action IN ('resource.rejected','resource.observed') AND created_at_ms>=? AND created_at_ms<?`
	args := []any{timeMS(from), timeMS(to)}
	if userID != "" {
		where += ` AND target_user_id=?`
		args = append(args, userID)
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`+where, args...).Scan(&count); err != nil {
		return nil, 0, err
	}
	args = append(args, max(1, min(limit, 100)))
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(actor_user_id,''),actor_label,action,COALESCE(target_user_id,''),target_label,detail,ip,created_at_ms FROM audit_logs`+where+` ORDER BY created_at_ms DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	events, err := scanAuditRows(rows)
	return events, count, err
}

// Count persisted events in the rolling seven-day window, including across restarts.
func (s *Store) ResourceCounts(ctx context.Context, now time.Time) (ResourceCounts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT action,COUNT(*) FROM audit_logs
		WHERE action IN ('resource.rejected','resource.observed') AND created_at_ms>=? AND created_at_ms<=?
		GROUP BY action`, timeMS(now.Add(-7*24*time.Hour)), timeMS(now))
	if err != nil {
		return ResourceCounts{}, err
	}
	defer rows.Close()
	var counts ResourceCounts
	for rows.Next() {
		var action string
		var count uint64
		if err := rows.Scan(&action, &count); err != nil {
			return ResourceCounts{}, err
		}
		if action == "resource.rejected" {
			counts.Rejected = count
		} else if action == "resource.observed" {
			counts.Observed = count
		}
	}
	return counts, rows.Err()
}

// A batch is atomic so a failed flush can be retried without duplicate rows.
func (s *Store) ResourceAuditBatch(ctx context.Context, events []domain.AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,actor_label,action,target_user_id,target_label,detail,ip,created_at_ms) VALUES(NULLIF(?,''),?,?,NULLIF(?,''),?,?,?,?)`, e.ActorUserID, e.ActorLabel, e.Action, e.TargetID, e.TargetLabel, e.Detail, e.IP, timeMS(e.CreatedAt))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
