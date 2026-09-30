package store

import (
	"context"
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
