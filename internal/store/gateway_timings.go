package store

import (
	"context"
	"errors"
	"time"
)

// GatewayRequestTiming contains only server-side timestamps and durations.
// It is saved independently of dialogue extraction, including empty/error bodies.
type GatewayRequestTiming struct {
	ID                string
	UserID            string
	APIKeyHash        string
	CPARequestID      string
	StartedAt         time.Time
	EndedAt           time.Time
	StartSource       string
	RequestReadMS     *int64
	UpstreamHeadersMS *int64
	ResponseStartedMS *int64
	TotalMS           int64
	Completed         bool
}

func (s *Store) SaveGatewayRequestTiming(ctx context.Context, row GatewayRequestTiming) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO gateway_request_timings
		(id,user_id,api_key_hash,cpa_request_id,started_at_ms,ended_at_ms,start_source,request_read_ms,upstream_headers_ms,response_started_ms,total_ms,completed)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, row.ID, row.UserID, row.APIKeyHash, row.CPARequestID, row.StartedAt.UnixMilli(), row.EndedAt.UnixMilli(), row.StartSource, row.RequestReadMS, row.UpstreamHeadersMS, row.ResponseStartedMS, row.TotalMS, row.Completed)
	if err != nil {
		return err
	}
	// Keep compact trend samples independently of the 100 detailed rows. They
	// contain neither conversation content nor API keys/request identifiers.
	if _, err = tx.ExecContext(ctx, `INSERT INTO gateway_timing_samples(id,user_id,started_at_ms,total_ms)
		VALUES(?,?,?,?)`, row.ID, row.UserID, row.StartedAt.UnixMilli(), row.TotalMS); err != nil {
		return err
	}
	// Keep the same per-user request window as dialogue records, independently
	// of whether a dialogue could be extracted or encrypted successfully.
	if _, err = tx.ExecContext(ctx, `DELETE FROM gateway_request_timings WHERE id IN
		(SELECT id FROM gateway_request_timings WHERE user_id=? ORDER BY started_at_ms DESC,id DESC LIMIT -1 OFFSET 100)`, row.UserID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GatewayTimingByCPARequest(ctx context.Context, requestID, apiKeyHash string) (GatewayRequestTiming, error) {
	var row GatewayRequestTiming
	var start, end int64
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,api_key_hash,cpa_request_id,started_at_ms,ended_at_ms,start_source,request_read_ms,upstream_headers_ms,response_started_ms,total_ms,completed
		FROM gateway_request_timings WHERE cpa_request_id=? AND api_key_hash=? ORDER BY started_at_ms DESC LIMIT 1`, requestID, apiKeyHash).
		Scan(&row.ID, &row.UserID, &row.APIKeyHash, &row.CPARequestID, &start, &end, &row.StartSource, &row.RequestReadMS, &row.UpstreamHeadersMS, &row.ResponseStartedMS, &row.TotalMS, &row.Completed)
	row.StartedAt, row.EndedAt = time.UnixMilli(start).UTC(), time.UnixMilli(end).UTC()
	return row, err
}

type GatewayTimingTotal struct {
	BucketMS int64
	Samples  int64
	TotalMS  int64
}

// GatewayTimingTotals filters exact range boundaries before grouping. Average
// durations must be weighted by sample count, never by the number of buckets.
func (s *Store) GatewayTimingTotals(ctx context.Context, from, to time.Time, userID string, anchor time.Time, bucket time.Duration) ([]GatewayTimingTotal, error) {
	if bucket.Milliseconds() <= 0 || anchor.After(from) {
		return nil, errors.New("invalid timing bucket or anchor")
	}
	query := `SELECT ((started_at_ms-?)/?)*?+? AS bucket_ms,COUNT(*),SUM(total_ms)
		FROM gateway_timing_samples WHERE started_at_ms>=? AND started_at_ms<?`
	args := []any{anchor.UnixMilli(), bucket.Milliseconds(), bucket.Milliseconds(), anchor.UnixMilli(), from.UnixMilli(), to.UnixMilli()}
	if userID != "" {
		query += ` AND user_id=?`
		args = append(args, userID)
	}
	query += ` GROUP BY bucket_ms ORDER BY bucket_ms`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var totals []GatewayTimingTotal
	for rows.Next() {
		var total GatewayTimingTotal
		if err := rows.Scan(&total.BucketMS, &total.Samples, &total.TotalMS); err != nil {
			return nil, err
		}
		totals = append(totals, total)
	}
	return totals, rows.Err()
}

func (s *Store) GatewayTimingSummary(ctx context.Context, from, to time.Time, userID string) (GatewayTimingTotal, error) {
	query := `SELECT COUNT(*),COALESCE(SUM(total_ms),0) FROM gateway_timing_samples WHERE started_at_ms>=? AND started_at_ms<?`
	args := []any{from.UnixMilli(), to.UnixMilli()}
	if userID != "" {
		query += ` AND user_id=?`
		args = append(args, userID)
	}
	var total GatewayTimingTotal
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&total.Samples, &total.TotalMS)
	return total, err
}
