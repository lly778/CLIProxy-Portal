package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"cliproxy-portal/internal/domain"
)

type OutcomeReadWindow struct {
	FromMS, ToMS, ReadAtMS     int64
	FailureCursor, AuditCursor int64
}

type OutcomeReadBatch struct {
	FailureCursor, AuditCursor int64
	HeadFromMS, EdgeToMS       int64
	Failures                   []domain.GatewayFailure
	Rejections                 []domain.RequestRejection
}

// A single SQLite snapshot covers both local failure results and rejections.
// Arrival cursors catch old requests saved late. Recent/boundary buckets are
// replaced, never appended, so overlap reads cannot double-count attempts.
func (s *Store) ReadUsageOutcomes(ctx context.Context, users []string, global bool, fromMS, toMS int64, previous *OutcomeReadWindow, overlap time.Duration) (OutcomeReadBatch, error) {
	var batch OutcomeReadBatch
	if fromMS <= 0 || toMS <= fromMS || overlap < 0 {
		return batch, errors.New("invalid outcome range")
	}
	if !global && len(users) == 0 {
		return batch, nil
	}
	payload, _ := json.Marshal(users)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return batch, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(MAX(rowid),0) FROM gateway_request_failures),(SELECT COALESCE(MAX(id),0) FROM audit_logs)`).Scan(&batch.FailureCursor, &batch.AuditCursor)
	if err != nil {
		return batch, err
	}
	batch.HeadFromMS = fromMS
	batch.EdgeToMS = fromMS
	if previous != nil {
		if fromMS < previous.FromMS || toMS < previous.ToMS || fromMS >= previous.ToMS {
			return batch, errors.New("invalid outcome checkpoint")
		}
		if batch.FailureCursor < previous.FailureCursor || batch.AuditCursor < previous.AuditCursor {
			return batch, nil
		}
		batch.HeadFromMS = max(fromMS, (previous.ToMS-overlap.Milliseconds())/60000*60000)
		batch.EdgeToMS = min(toMS, (fromMS/60000+1)*60000, batch.HeadFromMS)
	}
	columns := `id,user_id,api_key_hash,request_id,started_at_ms,ended_at_ms,status_code,failure_reason,checked_at_ms,generated_at_ms,upstream_json`
	args := []any{}
	failureSelect := func(extra string, extraArgs ...any) string {
		args = append(args, fromMS, toMS)
		query := `SELECT ` + columns + ` FROM gateway_request_failures`
		switch extra {
		case "rowid>?":
			query += ` NOT INDEXED`
		case "checked_at_ms>=?":
			query += ` INDEXED BY idx_gateway_failures_checked`
		case "started_at_ms>=?":
			query += ` INDEXED BY idx_gateway_failures_time`
		}
		query += ` WHERE ended_at_ms>=? AND started_at_ms<?`
		if !global {
			query += ` AND user_id IN (SELECT value FROM json_each(?))`
			args = append(args, string(payload))
		}
		if extra != "" {
			query += ` AND ` + extra
			args = append(args, extraArgs...)
		}
		return query
	}
	query := ""
	if previous == nil {
		query = failureSelect("")
	} else {
		query = failureSelect("rowid>?", previous.FailureCursor) + ` UNION ` +
			failureSelect("checked_at_ms>=?", max(1, previous.ReadAtMS-overlap.Milliseconds())) + ` UNION ` +
			failureSelect("started_at_ms>=?", batch.HeadFromMS)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return batch, err
	}
	for rows.Next() {
		var r domain.GatewayFailure
		var raw string
		if err = rows.Scan(&r.ID, &r.UserID, &r.APIKeyHash, &r.RequestID, &r.StartedMS, &r.EndedMS, &r.Status, &r.Reason, &r.CheckedMS, &r.GeneratedMS, &raw); err != nil {
			rows.Close()
			return batch, err
		}
		if err = json.Unmarshal([]byte(raw), &r.Upstream); err != nil {
			rows.Close()
			return batch, err
		}
		batch.Failures = append(batch.Failures, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return batch, err
	}
	args = nil
	scope := func(alias string) string {
		if global {
			return ""
		}
		args = append(args, string(payload))
		return ` AND ` + alias + `target_user_id IN (SELECT value FROM json_each(?))`
	}
	selected := func(from, to int64) string {
		args = append(args, from, to)
		index := ""
		if previous != nil {
			index = ` INDEXED BY idx_audit_created`
		}
		return `SELECT COALESCE(target_user_id,'') AS user_id,created_at_ms FROM audit_logs` + index + ` WHERE action='resource.rejected' AND created_at_ms>=? AND created_at_ms<?` + scope("")
	}
	prefix := "WITH "
	if previous == nil {
		query = selected(fromMS, toMS)
	} else {
		args = append(args, previous.AuditCursor, batch.EdgeToMS, batch.HeadFromMS)
		prefix += `late_buckets AS (SELECT DISTINCT created_at_ms/60000*60000 AS bucket_ms FROM audit_logs NOT INDEXED WHERE id>? AND action='resource.rejected' AND created_at_ms>=? AND created_at_ms<?` + scope("") + `),`
		query = selected(batch.HeadFromMS, toMS) + ` UNION ALL ` + selected(fromMS, batch.EdgeToMS)
		args = append(args, batch.EdgeToMS, batch.HeadFromMS)
		query += ` UNION ALL SELECT COALESCE(sample.target_user_id,''),sample.created_at_ms FROM late_buckets CROSS JOIN audit_logs AS sample INDEXED BY idx_audit_created WHERE sample.action='resource.rejected' AND sample.created_at_ms>=late_buckets.bucket_ms AND sample.created_at_ms<late_buckets.bucket_ms+60000 AND sample.created_at_ms>=? AND sample.created_at_ms<?` + scope("sample.")
	}
	rows, err = tx.QueryContext(ctx, prefix+`selected AS (`+query+`) SELECT user_id,created_at_ms/60000*60000,COUNT(*) FROM selected GROUP BY 1,2`, args...)
	if err != nil {
		return batch, err
	}
	for rows.Next() {
		var r domain.RequestRejection
		if err = rows.Scan(&r.UserID, &r.AtMS, &r.Calls); err != nil {
			rows.Close()
			return batch, err
		}
		batch.Rejections = append(batch.Rejections, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return batch, err
	}
	return batch, tx.Commit()
}
