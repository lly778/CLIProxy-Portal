package store

import (
	"cliproxy-portal/internal/domain"
	"context"
)

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
