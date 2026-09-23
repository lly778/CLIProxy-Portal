package store

import (
	"context"
	"database/sql"
	"time"
)

// GatewayCapture is metadata only. Request and response bodies are stored as
// encrypted files outside SQLite so normal database queries never load them.
type GatewayCapture struct {
	ID                  string
	UserID              string
	APIKeyHash          string
	CPARequestID        string
	CreatedAt           time.Time
	Method              string
	Path                string
	RequestedModel      string
	StatusCode          int
	RequestContentType  string
	ResponseContentType string
	RequestTruncated    bool
	ResponseTruncated   bool
}

const gatewayCaptureColumns = `id,user_id,api_key_hash,cpa_request_id,created_at_ms,method,path,requested_model,status_code,request_content_type,response_content_type,request_truncated,response_truncated`

// GatewayCaptureMessage references one encrypted, per-user shared message.
type GatewayCaptureMessage struct {
	Side string
	ID   string
	Role string
}

type GatewayCaptureDeletion struct {
	CaptureIDs []string
	MessageIDs []string
}

func scanGatewayCapture(scanner interface{ Scan(...any) error }) (GatewayCapture, error) {
	var row GatewayCapture
	var created int64
	var requestTruncated, responseTruncated int
	err := scanner.Scan(&row.ID, &row.UserID, &row.APIKeyHash, &row.CPARequestID, &created, &row.Method, &row.Path, &row.RequestedModel, &row.StatusCode, &row.RequestContentType, &row.ResponseContentType, &requestTruncated, &responseTruncated)
	row.CreatedAt = time.UnixMilli(created).UTC()
	row.RequestTruncated = requestTruncated != 0
	row.ResponseTruncated = responseTruncated != 0
	return row, err
}

func (s *Store) SaveGatewayCapture(ctx context.Context, row GatewayCapture) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO gateway_captures (`+gatewayCaptureColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.ID, row.UserID, row.APIKeyHash, row.CPARequestID, row.CreatedAt.UnixMilli(), row.Method, row.Path, row.RequestedModel, row.StatusCode, row.RequestContentType, row.ResponseContentType, row.RequestTruncated, row.ResponseTruncated)
	return err
}

func (s *Store) SaveGatewayCaptureWithMessages(ctx context.Context, row GatewayCapture, messages []GatewayCaptureMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_captures (`+gatewayCaptureColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.ID, row.UserID, row.APIKeyHash, row.CPARequestID, row.CreatedAt.UnixMilli(), row.Method, row.Path, row.RequestedModel, row.StatusCode, row.RequestContentType, row.ResponseContentType, row.RequestTruncated, row.ResponseTruncated); err != nil {
		return err
	}
	for position, message := range messages {
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_capture_messages(capture_id,side,position,message_id,role) VALUES(?,?,?,?,?)`, row.ID, message.Side, position, message.ID, message.Role); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GatewayCaptureMessages(ctx context.Context, captureID, side string) ([]GatewayCaptureMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id,role FROM gateway_capture_messages WHERE capture_id=? AND side=? ORDER BY position`, captureID, side)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []GatewayCaptureMessage
	for rows.Next() {
		var message GatewayCaptureMessage
		if err := rows.Scan(&message.ID, &message.Role); err != nil {
			return nil, err
		}
		message.Side = side
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) GatewayCaptureByID(ctx context.Context, id string) (GatewayCapture, error) {
	return scanGatewayCapture(s.db.QueryRowContext(ctx, `SELECT `+gatewayCaptureColumns+` FROM gateway_captures WHERE id=?`, id))
}

func (s *Store) GatewayCaptureIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM gateway_captures`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func (s *Store) ReferencedGatewayMessageIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT message_id FROM gateway_capture_messages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func (s *Store) GatewayCaptureByCPARequest(ctx context.Context, requestID, apiKeyHash string) (GatewayCapture, error) {
	return scanGatewayCapture(s.db.QueryRowContext(ctx, `SELECT `+gatewayCaptureColumns+` FROM gateway_captures WHERE cpa_request_id=? AND api_key_hash=? ORDER BY created_at_ms DESC LIMIT 1`, requestID, apiKeyHash))
}

func (s *Store) ListGatewayCaptures(ctx context.Context, userID string, limit int) ([]GatewayCapture, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	query := `SELECT ` + gatewayCaptureColumns + ` FROM gateway_captures`
	args := []any{}
	if userID != "" {
		query += ` WHERE user_id=?`
		args = append(args, userID)
	}
	query += ` ORDER BY created_at_ms DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]GatewayCapture, 0)
	for rows.Next() {
		row, scanErr := scanGatewayCapture(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// DeleteExpiredGatewayCaptures returns capture files and now-unreferenced
// shared message files that can be removed after the transaction commits.
func (s *Store) DeleteExpiredGatewayCaptures(ctx context.Context, before time.Time) (GatewayCaptureDeletion, error) {
	return s.deleteGatewayCaptures(ctx, `SELECT id FROM gateway_captures WHERE created_at_ms<?`, before.UnixMilli())
}

// DeleteExcessGatewayCaptures keeps the newest limit rows for one user.
func (s *Store) DeleteExcessGatewayCaptures(ctx context.Context, userID string, limit int) (GatewayCaptureDeletion, error) {
	if limit < 0 {
		limit = 0
	}
	return s.deleteGatewayCaptures(ctx, `SELECT id FROM gateway_captures WHERE user_id=? ORDER BY created_at_ms DESC, id DESC LIMIT -1 OFFSET ?`, userID, limit)
}

func (s *Store) deleteGatewayCaptures(ctx context.Context, query string, args ...any) (GatewayCaptureDeletion, error) {
	var deleted GatewayCaptureDeletion
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return deleted, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return deleted, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return deleted, err
		}
		deleted.CaptureIDs = append(deleted.CaptureIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return deleted, err
	}
	_ = rows.Close()
	if len(deleted.CaptureIDs) == 0 {
		return deleted, nil
	}
	candidates := make(map[string]bool)
	for _, id := range deleted.CaptureIDs {
		messageRows, err := tx.QueryContext(ctx, `SELECT message_id FROM gateway_capture_messages WHERE capture_id=?`, id)
		if err != nil {
			return deleted, err
		}
		for messageRows.Next() {
			var messageID string
			if err := messageRows.Scan(&messageID); err != nil {
				_ = messageRows.Close()
				return deleted, err
			}
			candidates[messageID] = true
		}
		if err := messageRows.Err(); err != nil {
			_ = messageRows.Close()
			return deleted, err
		}
		_ = messageRows.Close()
		if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_captures WHERE id=?`, id); err != nil {
			return deleted, err
		}
	}
	for messageID := range candidates {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM gateway_capture_messages WHERE message_id=? LIMIT 1`, messageID).Scan(&exists)
		if err == sql.ErrNoRows {
			deleted.MessageIDs = append(deleted.MessageIDs, messageID)
		} else if err != nil {
			return deleted, err
		}
	}
	if err := tx.Commit(); err != nil {
		return GatewayCaptureDeletion{}, err
	}
	return deleted, nil
}
