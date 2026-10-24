package store

import (
	"context"
	"errors"
	"time"
)

const TimingSampleBucketMS = int64(time.Minute / time.Millisecond)

type TimingReadWindow struct {
	FromMS, ToMS, Cursor int64
}

type TimingReadBatch struct {
	Cursor int64
	// A nil previous window performs one full scan; a delta re-reads recent
	// minute buckets, the lower edge and buckets containing newly inserted IDs.
	HeadFromMS int64
	Totals     []GatewayTimingTotal
}

// ReadUsageTimings combines summary/trend work in one SQL statement and one
// SQLite read snapshot. rowid tracks arrival, not request start: a slow request
// saved hours after it started still updates its historical minute bucket.
func (s *Store) ReadUsageTimings(ctx context.Context, fromMS, toMS int64, userID string, previous *TimingReadWindow, overlap time.Duration) (TimingReadBatch, error) {
	var batch TimingReadBatch
	if fromMS <= 0 || toMS <= fromMS || overlap < 0 {
		return batch, errors.New("invalid usage timing range")
	}
	columns := "started_at_ms,total_ms,request_read_ms,response_started_ms"
	var args []any
	userFilter := func(alias string) string {
		if userID == "" {
			return ""
		}
		args = append(args, userID)
		return " AND " + alias + "user_id=?"
	}
	selected := ""
	prefix := `WITH current_cursor AS (SELECT COALESCE(MAX(rowid),0) AS cursor FROM gateway_timing_samples),`
	if previous == nil {
		batch.HeadFromMS = fromMS
		args = append(args, fromMS, toMS)
		selected = `SELECT ` + columns + ` FROM gateway_timing_samples WHERE started_at_ms>=? AND started_at_ms<?` + userFilter("")
	} else {
		if previous.Cursor < 0 || fromMS < previous.FromMS || toMS < previous.ToMS || fromMS >= previous.ToMS {
			return batch, errors.New("invalid timing checkpoint")
		}
		batch.HeadFromMS = max(fromMS, (previous.ToMS-overlap.Milliseconds())/TimingSampleBucketMS*TimingSampleBucketMS)
		edgeTo := min(toMS, (fromMS/TimingSampleBucketMS+1)*TimingSampleBucketMS, batch.HeadFromMS)
		// Each UNION branch uses a time-index range; the late-bucket lookup
		// uses SQLite's rowid index. Historical rows outside these buckets are
		// neither returned nor scanned as part of a per-refresh SUM/COUNT.
		args = append(args, previous.Cursor, edgeTo, batch.HeadFromMS)
		prefix += ` late_buckets AS (SELECT DISTINCT started_at_ms/60000*60000 AS bucket_ms FROM gateway_timing_samples NOT INDEXED
			WHERE rowid>? AND started_at_ms>=? AND started_at_ms<?` + userFilter("") + `),`
		args = append(args, batch.HeadFromMS, toMS)
		selected = `SELECT ` + columns + ` FROM gateway_timing_samples WHERE started_at_ms>=? AND started_at_ms<?` + userFilter("")
		args = append(args, fromMS, edgeTo)
		selected += ` UNION ALL SELECT ` + columns + ` FROM gateway_timing_samples WHERE started_at_ms>=? AND started_at_ms<?` + userFilter("")
		args = append(args, edgeTo, batch.HeadFromMS)
		selected += ` UNION ALL SELECT sample.started_at_ms,sample.total_ms,sample.request_read_ms,sample.response_started_ms
			FROM late_buckets CROSS JOIN gateway_timing_samples AS sample
			WHERE sample.started_at_ms>=late_buckets.bucket_ms AND sample.started_at_ms<late_buckets.bucket_ms+60000
			AND sample.started_at_ms>=? AND sample.started_at_ms<?` + userFilter("sample.")
	}
	query := prefix + ` selected AS (` + selected + `), totals AS (
		SELECT started_at_ms/60000*60000 AS bucket_ms,COUNT(*) AS samples,SUM(total_ms) AS total_ms` + timingStageSums + `
		FROM selected GROUP BY bucket_ms)
		SELECT current_cursor.cursor,totals.* FROM totals CROSS JOIN current_cursor
		UNION ALL SELECT cursor,-1,0,0,0,0,0,0 FROM current_cursor`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return batch, err
	}
	defer rows.Close()
	for rows.Next() {
		var total GatewayTimingTotal
		if err := rows.Scan(&batch.Cursor, &total.BucketMS, &total.Samples, &total.TotalMS, &total.StageSamples, &total.UploadMS, &total.WaitMS, &total.ResponseMS); err != nil {
			return TimingReadBatch{}, err
		}
		if total.BucketMS >= 0 {
			batch.Totals = append(batch.Totals, total)
		}
	}
	return batch, rows.Err()
}
