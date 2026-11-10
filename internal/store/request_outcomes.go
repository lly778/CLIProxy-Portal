package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"cliproxy-portal/internal/domain"
)

func (s *Store) migrateRequestOutcomes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS gateway_request_failures (
		id TEXT PRIMARY KEY,user_id TEXT NOT NULL,api_key_hash TEXT NOT NULL,request_id TEXT NOT NULL,
		started_at_ms INTEGER NOT NULL,ended_at_ms INTEGER NOT NULL,status_code INTEGER NOT NULL,failure_reason TEXT NOT NULL,
		checked_at_ms INTEGER NOT NULL DEFAULT 0,generated_at_ms INTEGER NOT NULL DEFAULT 0,upstream_json TEXT NOT NULL DEFAULT '[]');
		CREATE INDEX IF NOT EXISTS idx_gateway_failures_time ON gateway_request_failures(started_at_ms);
		CREATE INDEX IF NOT EXISTS idx_gateway_failures_end ON gateway_request_failures(ended_at_ms,started_at_ms);
		CREATE INDEX IF NOT EXISTS idx_gateway_failures_user ON gateway_request_failures(user_id,started_at_ms);
		CREATE INDEX IF NOT EXISTS idx_gateway_failures_request ON gateway_request_failures(request_id,api_key_hash);
		CREATE INDEX IF NOT EXISTS idx_gateway_failures_checked ON gateway_request_failures(checked_at_ms);
		INSERT OR IGNORE INTO gateway_request_failures(id,user_id,api_key_hash,request_id,started_at_ms,ended_at_ms,status_code,failure_reason)
		SELECT id,user_id,lower(trim(api_key_hash)),lower(trim(cpa_request_id)),started_at_ms,MAX(ended_at_ms,started_at_ms+total_ms),status_code,
		CASE WHEN failure_reason<>'' THEN failure_reason WHEN status_code>=400 THEN 'upstream_request_failed' ELSE 'connection_interrupted' END
		FROM gateway_request_timings WHERE ((completed=0 AND (status_code<>0 OR cpa_request_id<>'' OR failure_reason<>'')) OR status_code>=400 OR failure_reason<>'') AND cpa_request_id NOT LIKE 'agy:%'`)
	return err
}

func saveGatewayFailure(ctx context.Context, tx *sql.Tx, row GatewayRequestTiming) error {
	if (row.Completed || row.StatusCode == 0 && row.CPARequestID == "" && row.FailureReason == "") && row.StatusCode < 400 && row.FailureReason == "" {
		return nil
	}
	reason := row.FailureReason
	if reason == "" {
		if row.StatusCode >= 400 {
			reason = "upstream_request_failed"
		} else {
			reason = "connection_interrupted"
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO gateway_request_failures(id,user_id,api_key_hash,request_id,started_at_ms,ended_at_ms,status_code,failure_reason) VALUES(?,?,?,?,?,?,?,?)`, row.ID, row.UserID, strings.ToLower(row.APIKeyHash), strings.ToLower(strings.TrimSpace(row.CPARequestID)), timeMS(row.StartedAt), max(timeMS(row.EndedAt), timeMS(row.StartedAt)+row.TotalMS), row.StatusCode, reason)
	return err
}

func (s *Store) RequestLogData(ctx context.Context, users []string, global bool, fromMS, toMS int64, timeline bool) (domain.RequestLogData, error) {
	var result domain.RequestLogData
	if !global && len(users) == 0 {
		return result, nil
	}
	payload, _ := json.Marshal(users)
	where := `ended_at_ms>=? AND started_at_ms<?`
	args := []any{fromMS, toMS}
	if !global {
		where += ` AND user_id IN (SELECT value FROM json_each(?))`
		args = append(args, string(payload))
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,user_id,api_key_hash,request_id,started_at_ms,ended_at_ms,status_code,failure_reason,checked_at_ms,generated_at_ms,upstream_json FROM gateway_request_failures WHERE `+where, args...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var r domain.GatewayFailure
		var raw string
		if err = rows.Scan(&r.ID, &r.UserID, &r.APIKeyHash, &r.RequestID, &r.StartedMS, &r.EndedMS, &r.Status, &r.Reason, &r.CheckedMS, &r.GeneratedMS, &raw); err != nil {
			rows.Close()
			return result, err
		}
		if err = json.Unmarshal([]byte(raw), &r.Upstream); err != nil {
			rows.Close()
			return result, err
		}
		result.Failures = append(result.Failures, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	where = `action='resource.rejected' AND created_at_ms>=? AND created_at_ms<?`
	args = []any{fromMS, toMS}
	if !global {
		where += ` AND target_user_id IN (SELECT value FROM json_each(?))`
		args = append(args, string(payload))
	}
	bucket := `?`
	args = append([]any{fromMS}, args...)
	if timeline {
		bucket = `created_at_ms/60000*60000`
		args = args[1:]
	}
	rows, err = tx.QueryContext(ctx, `SELECT COALESCE(target_user_id,''),`+bucket+`,COUNT(*) FROM audit_logs WHERE `+where+` GROUP BY 1,2`, args...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var r domain.RequestRejection
		if err = rows.Scan(&r.UserID, &r.AtMS, &r.Calls); err != nil {
			rows.Close()
			return result, err
		}
		result.Rejections = append(result.Rejections, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Store) SaveFailureReconciliation(ctx context.Context, failures []domain.GatewayFailure) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range failures {
		raw, err := json.Marshal(r.Upstream)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE gateway_request_failures SET checked_at_ms=?,generated_at_ms=?,upstream_json=? WHERE id=?`, r.CheckedMS, r.GeneratedMS, string(raw), r.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Exact request/key lookup for the displayed page only. No time/name guessing.
func (s *Store) FailedRequestReasons(ctx context.Context, identities [][2]string) (map[[2]string]string, error) {
	result := make(map[[2]string]string)
	if len(identities) == 0 {
		return result, nil
	}
	payload, _ := json.Marshal(identities)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT f.request_id,f.api_key_hash,f.failure_reason FROM json_each(?) j JOIN gateway_request_failures f ON f.request_id=json_extract(j.value,'$[0]') AND f.api_key_hash=json_extract(j.value,'$[1]')`, string(payload))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, reason string
		if err := rows.Scan(&id, &key, &reason); err != nil {
			return result, err
		}
		result[[2]string{id, key}] = reason
	}
	return result, rows.Err()
}
