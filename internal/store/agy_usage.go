package store

import (
	"cliproxy-portal/internal/agy"
	"context"
	"database/sql"
	"encoding/json"
)

type AGYUsage struct {
	Model, Requested, Effort, ResponseID string
	Usage                                agy.Usage
}

func (s *Store) migrateAGY(ctx context.Context) error {
	_, e := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agy_requests (
 id INTEGER PRIMARY KEY AUTOINCREMENT,timing_id TEXT NOT NULL UNIQUE,user_id TEXT NOT NULL,key_hash TEXT NOT NULL,
 started_ms INTEGER NOT NULL,ended_ms INTEGER NOT NULL,model TEXT NOT NULL,requested_model TEXT NOT NULL,effort TEXT NOT NULL,
 response_id TEXT NOT NULL,status INTEGER NOT NULL,failed INTEGER NOT NULL,reason TEXT NOT NULL,duration_ms INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL,output_tokens INTEGER NOT NULL,cached_tokens INTEGER NOT NULL,reasoning_tokens INTEGER NOT NULL,total_tokens INTEGER NOT NULL,usage_known INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_agy_request_time ON agy_requests(started_ms DESC,id DESC);
 CREATE INDEX IF NOT EXISTS idx_agy_request_key ON agy_requests(key_hash,started_ms DESC,id DESC);
 CREATE INDEX IF NOT EXISTS idx_agy_request_response ON agy_requests(key_hash,response_id) WHERE response_id<>'';
 CREATE TABLE IF NOT EXISTS agy_usage_minutes (
 minute_ms INTEGER NOT NULL,key_hash TEXT NOT NULL,model TEXT NOT NULL,calls INTEGER NOT NULL,success INTEGER NOT NULL,failures INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL,output_tokens INTEGER NOT NULL,cached_tokens INTEGER NOT NULL,reasoning_tokens INTEGER NOT NULL,total_tokens INTEGER NOT NULL,unknown_usage INTEGER NOT NULL,total_duration_ms INTEGER NOT NULL,last_seen_ms INTEGER NOT NULL,
 PRIMARY KEY(minute_ms,key_hash,model));
 CREATE INDEX IF NOT EXISTS idx_agy_usage_key ON agy_usage_minutes(key_hash,minute_ms);`)
	return e
}
func saveAGYUsage(ctx context.Context, tx *sql.Tx, row GatewayRequestTiming) error {
	if row.AGY == nil {
		return nil
	}
	v := row.AGY
	u := v.Usage
	failed := !row.Completed || row.StatusCode >= 400 || row.FailureReason != ""
	status := row.StatusCode
	if status == 0 {
		status = 502
	}
	// A replay is still an HTTP attempt, but re-delivering the same response
	// must not count its model tokens twice. This indexed check is in the same
	// write transaction as the request and minute aggregate updates.
	if u.Known && v.ResponseID != "" {
		var recorded bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agy_requests WHERE key_hash=? AND response_id=? AND response_id<>'' AND usage_known=1)`, row.APIKeyHash, v.ResponseID).Scan(&recorded); err != nil {
			return err
		}
		if recorded {
			u = agy.Usage{Known: true}
		}
	}
	result, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agy_requests(timing_id,user_id,key_hash,started_ms,ended_ms,model,requested_model,effort,response_id,status,failed,reason,duration_ms,input_tokens,output_tokens,cached_tokens,reasoning_tokens,total_tokens,usage_known) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.ID, row.UserID, row.APIKeyHash, row.StartedAt.UnixMilli(), row.EndedAt.UnixMilli(), v.Model, v.Requested, v.Effort, v.ResponseID, status, failed, row.FailureReason, row.TotalMS, u.Input, u.Output, u.Cache, u.Reasoning, u.Total, u.Known)
	if e != nil {
		return e
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return nil
	}
	success, failure, unknown := 0, 0, 0
	if failed {
		failure = 1
	} else {
		success = 1
	}
	if !u.Known {
		unknown = 1
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO agy_usage_minutes(minute_ms,key_hash,model,calls,success,failures,input_tokens,output_tokens,cached_tokens,reasoning_tokens,total_tokens,unknown_usage,total_duration_ms,last_seen_ms) VALUES(?,?,?,1,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(minute_ms,key_hash,model) DO UPDATE SET calls=calls+1,success=success+excluded.success,failures=failures+excluded.failures,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,cached_tokens=cached_tokens+excluded.cached_tokens,reasoning_tokens=reasoning_tokens+excluded.reasoning_tokens,total_tokens=total_tokens+excluded.total_tokens,unknown_usage=unknown_usage+excluded.unknown_usage,total_duration_ms=total_duration_ms+excluded.total_duration_ms,last_seen_ms=MAX(last_seen_ms,excluded.last_seen_ms)`, row.StartedAt.UnixMilli()/60000*60000, row.APIKeyHash, v.Model, success, failure, u.Input, u.Output, u.Cache, u.Reasoning, u.Total, unknown, row.TotalMS, row.StartedAt.UnixMilli())
	return e
}

type AGYAggregate struct {
	At                                                                                       int64
	Key, Model                                                                               string
	Calls, Success, Failure, Input, Output, Cache, Reasoning, Total, Unknown, Duration, Last int64
}

// Read only requested keys and aggregate rows. Boundary minutes use exact raw
// request timestamps; interior minutes use aggregates maintained on insertion.
func (s *Store) AGYAggregates(ctx context.Context, from, to int64, hashes []string, models ...string) ([]AGYAggregate, error) {
	filter := ""
	args := []any{from, to}
	if len(hashes) > 0 {
		b, _ := json.Marshal(hashes)
		filter = ` AND key_hash IN (SELECT value FROM json_each(?))`
		args = append(args, string(b))
	}
	if len(models) > 0 {
		b, _ := json.Marshal(models)
		filter += ` AND model IN (SELECT value FROM json_each(?))`
		args = append(args, string(b))
	}
	filterArgs := append([]any(nil), args[2:]...)
	q := `SELECT started_ms/60000*60000,key_hash,model,COUNT(*),SUM(1-failed),SUM(failed),SUM(input_tokens),SUM(output_tokens),SUM(cached_tokens),SUM(reasoning_tokens),SUM(total_tokens),SUM(1-usage_known),SUM(duration_ms),MAX(started_ms) FROM agy_requests WHERE started_ms>=? AND started_ms<?` + filter + ` AND (started_ms<? OR started_ms>=?) GROUP BY 1,key_hash,model`
	lo := (from + 59999) / 60000 * 60000
	hi := to / 60000 * 60000
	args = append(args, lo, hi)
	q += ` UNION ALL SELECT minute_ms,key_hash,model,calls,success,failures,input_tokens,output_tokens,cached_tokens,reasoning_tokens,total_tokens,unknown_usage,total_duration_ms,last_seen_ms FROM agy_usage_minutes WHERE minute_ms>=? AND minute_ms<?`
	args = append(args, lo, hi)
	q += filter
	args = append(args, filterArgs...)

	q += ` UNION ALL SELECT at_ms/60000*60000,key_hash,model,0,0,0,SUM(input_tokens),SUM(output_tokens),SUM(cached_tokens),SUM(reasoning_tokens),SUM(total_tokens),SUM(1-usage_known),0,MAX(at_ms) FROM agy_summary_usage WHERE at_ms>=? AND at_ms<?` + filter + ` GROUP BY 1,key_hash,model`
	args = append(args, from, to)
	args = append(args, filterArgs...)
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []AGYAggregate
	for rows.Next() {
		var v AGYAggregate
		if e = rows.Scan(&v.At, &v.Key, &v.Model, &v.Calls, &v.Success, &v.Failure, &v.Input, &v.Output, &v.Cache, &v.Reasoning, &v.Total, &v.Unknown, &v.Duration, &v.Last); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type AGYRequestRow struct {
	ID, At, Duration                                                  int64
	TimingID, User, Key, Model, Requested, Effort, ResponseID, Reason string
	Status                                                            int
	Failed                                                            bool
	Usage                                                             agy.Usage
}

func (s *Store) AGYRequestPage(ctx context.Context, from, to int64, hashes []string, limit int, models ...string) ([]AGYRequestRow, int64, error) {
	where := ` WHERE started_ms>=? AND started_ms<?`
	args := []any{from, to}
	if len(hashes) > 0 {
		b, _ := json.Marshal(hashes)
		where += ` AND key_hash IN (SELECT value FROM json_each(?))`
		args = append(args, string(b))
	}
	if len(models) > 0 {
		b, _ := json.Marshal(models)
		where += ` AND model IN (SELECT value FROM json_each(?))`
		args = append(args, string(b))
	}
	var total int64
	if e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agy_requests`+where, args...).Scan(&total); e != nil {
		return nil, 0, e
	}
	args = append(args, min(max(limit, 1), 5000))
	rows, e := s.db.QueryContext(ctx, `SELECT id,timing_id,user_id,key_hash,started_ms,model,requested_model,effort,response_id,status,failed,reason,duration_ms,input_tokens,output_tokens,cached_tokens,reasoning_tokens,total_tokens,usage_known FROM agy_requests`+where+` ORDER BY started_ms DESC,id DESC LIMIT ?`, args...)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	var out []AGYRequestRow
	for rows.Next() {
		var v AGYRequestRow
		if e = rows.Scan(&v.ID, &v.TimingID, &v.User, &v.Key, &v.At, &v.Model, &v.Requested, &v.Effort, &v.ResponseID, &v.Status, &v.Failed, &v.Reason, &v.Duration, &v.Usage.Input, &v.Usage.Output, &v.Usage.Cache, &v.Usage.Reasoning, &v.Usage.Total, &v.Usage.Known); e != nil {
			return nil, 0, e
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}
