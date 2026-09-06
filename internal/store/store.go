package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"cliproxy-portal/internal/domain"
	_ "modernc.org/sqlite"
)

var ErrNotFound = sql.ErrNoRows

type Store struct{ db *sql.DB }

type Session struct {
	TokenHash string
	UserID    string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
	IP        string
	UserAgent string
}

type PasswordReset struct {
	CodeHash  string
	UserID    string
	ExpiresAt time.Time
	UsedAt    time.Time
	CreatedAt time.Time
}

func Open(path string, registrationOpen bool) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background(), registrationOpen); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context, registrationOpen bool) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			phone TEXT UNIQUE,
			name TEXT,
			password_hash TEXT,
			role TEXT NOT NULL CHECK(role IN ('user','admin')),
			status TEXT NOT NULL,
			rejection_reason TEXT NOT NULL DEFAULT '',
			suspension_reason TEXT NOT NULL DEFAULT '',
			policy_version INTEGER NOT NULL DEFAULT 0,
			policy_accepted_at_ms INTEGER NOT NULL DEFAULT 0,
			approved_at_ms INTEGER NOT NULL DEFAULT 0,
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			last_login_at_ms INTEGER NOT NULL DEFAULT 0,
			deleted_at_ms INTEGER NOT NULL DEFAULT 0,
			issue_cooldown_until_ms INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_users_status ON users(status)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id),
			key_hash TEXT NOT NULL UNIQUE,
			last_four TEXT NOT NULL,
			alias TEXT NOT NULL,
			status TEXT NOT NULL,
			issued_at_ms INTEGER NOT NULL,
			revoked_at_ms INTEGER NOT NULL DEFAULT 0,
			last_seen_at_ms INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id, issued_at_ms DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_one_active_key ON api_keys(user_id) WHERE status = 'active'`,
		`CREATE TABLE IF NOT EXISTS model_test_results (
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			model TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('success','warning','danger')),
			message TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER NOT NULL DEFAULT 0,
			tested_at_ms INTEGER NOT NULL,
			PRIMARY KEY(user_id, model)
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at_ms INTEGER NOT NULL,
			last_seen_at_ms INTEGER NOT NULL,
			expires_at_ms INTEGER NOT NULL,
			ip TEXT NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		`CREATE TABLE IF NOT EXISTS password_resets (
			code_hash TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at_ms INTEGER NOT NULL,
			used_at_ms INTEGER NOT NULL DEFAULT 0,
			created_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			actor_user_id TEXT,
			actor_label TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			target_user_id TEXT,
			target_label TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at_ms DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_logs(actor_user_id,id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_logs(target_user_id,id DESC)`,
		`CREATE TABLE IF NOT EXISTS policies (
			version INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			created_by TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sync_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			action TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_at_ms INTEGER NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL,
			UNIQUE(user_id, action, key_hash)
		)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	open := "false"
	if registrationOpen {
		open = "true"
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO settings(key,value) VALUES('registration_open',?)`, open); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO policies(title,body,created_at_ms)
		SELECT 'CLIProxy 使用规则','仅限本人使用，禁止共享 API Key；请遵守公司数据与信息安全要求。',?
		WHERE NOT EXISTS (SELECT 1 FROM policies)`, nowMS()); err != nil {
		return err
	}
	return nil
}

func nowMS() int64 { return time.Now().UTC().UnixMilli() }
func timeMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}
func fromMS(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func (s *Store) RegistrationOpen(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='registration_open'`).Scan(&value)
	return value == "true", err
}

func (s *Store) SetRegistrationOpen(ctx context.Context, open bool) error {
	v := "false"
	if open {
		v = "true"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('registration_open',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, v)
	return err
}

func (s *Store) LatestPolicy(ctx context.Context) (domain.Policy, error) {
	var p domain.Policy
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT version,title,body,created_by,created_at_ms FROM policies ORDER BY version DESC LIMIT 1`).Scan(&p.Version, &p.Title, &p.Body, &p.CreatedBy, &created)
	p.CreatedAt = fromMS(created)
	return p, err
}

func (s *Store) SavePolicy(ctx context.Context, title, body, actor string) (domain.Policy, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO policies(title,body,created_by,created_at_ms) VALUES(?,?,?,?)`, title, body, actor, nowMS())
	if err != nil {
		return domain.Policy{}, err
	}
	id, _ := res.LastInsertId()
	return domain.Policy{Version: int(id), Title: title, Body: body, CreatedBy: actor, CreatedAt: time.Now().UTC()}, nil
}

func (s *Store) ListPolicies(ctx context.Context, limit int) ([]domain.Policy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version,title,body,created_by,created_at_ms FROM policies ORDER BY version DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Policy
	for rows.Next() {
		var p domain.Policy
		var created int64
		if err := rows.Scan(&p.Version, &p.Title, &p.Body, &p.CreatedBy, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = fromMS(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreateUser(ctx context.Context, u domain.User) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id,phone,name,password_hash,role,status,policy_version,policy_accepted_at_ms,approved_at_ms,created_at_ms,updated_at_ms,issue_cooldown_until_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, u.ID, u.Phone, u.Name, u.PasswordHash, u.Role, u.Status, u.PolicyVersion, timeMS(u.PolicyAcceptedAt), timeMS(u.ApprovedAt), timeMS(u.CreatedAt), timeMS(u.UpdatedAt), timeMS(u.IssueCooldownUntil))
	return err
}

const userColumns = `id,COALESCE(phone,''),COALESCE(name,''),COALESCE(password_hash,''),role,status,rejection_reason,suspension_reason,policy_version,policy_accepted_at_ms,approved_at_ms,created_at_ms,updated_at_ms,last_login_at_ms,deleted_at_ms,issue_cooldown_until_ms`

func scanUser(scanner interface{ Scan(...any) error }) (domain.User, error) {
	var u domain.User
	var accepted, approved, created, updated, lastLogin, deleted, cooldown int64
	err := scanner.Scan(&u.ID, &u.Phone, &u.Name, &u.PasswordHash, &u.Role, &u.Status, &u.RejectionReason, &u.SuspensionReason, &u.PolicyVersion, &accepted, &approved, &created, &updated, &lastLogin, &deleted, &cooldown)
	u.PolicyAcceptedAt = fromMS(accepted)
	u.ApprovedAt = fromMS(approved)
	u.CreatedAt = fromMS(created)
	u.UpdatedAt = fromMS(updated)
	u.LastLoginAt = fromMS(lastLogin)
	u.DeletedAt = fromMS(deleted)
	u.IssueCooldownUntil = fromMS(cooldown)
	return u, err
}

func (s *Store) UserByPhone(ctx context.Context, phone string) (domain.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE phone=? AND status<>'deleted'`, phone))
}
func (s *Store) UserByID(ctx context.Context, id string) (domain.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id))
}

func (s *Store) ListUsers(ctx context.Context, status, query string, limit, offset int) ([]domain.User, error) {
	where := `status<>'deleted'`
	args := []any{}
	if status != "" {
		where += ` AND status=?`
		args = append(args, status)
	}
	if strings.TrimSpace(query) != "" {
		where += ` AND (phone LIKE ? OR name LIKE ?)`
		q := "%" + strings.TrimSpace(query) + "%"
		args = append(args, q, q)
	}
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users WHERE `+where+` ORDER BY created_at_ms DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []domain.User
	for rows.Next() {
		u, e := scanUser(rows)
		if e != nil {
			return nil, e
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context, status string) (int, error) {
	q := `SELECT COUNT(*) FROM users WHERE status<>'deleted'`
	args := []any{}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

func (s *Store) ApproveUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET status='approved',rejection_reason='',suspension_reason='',approved_at_ms=?,updated_at_ms=? WHERE id=? AND status IN ('pending','rejected','suspended')`, nowMS(), nowMS(), id)
	return err
}
func (s *Store) RejectUser(ctx context.Context, id, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET status='rejected',rejection_reason=?,updated_at_ms=? WHERE id=? AND status IN ('pending','rejected')`, reason, nowMS(), id)
	return err
}
func (s *Store) SetUserStatus(ctx context.Context, id string, status domain.UserStatus, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET status=?,suspension_reason=?,updated_at_ms=? WHERE id=?`, status, reason, nowMS(), id)
	return err
}
func (s *Store) SetUserPhone(ctx context.Context, id, phone string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET phone=?,updated_at_ms=? WHERE id=? AND status<>'deleted'`, phone, nowMS(), id)
	return err
}
func (s *Store) ResubmitRejectedUser(ctx context.Context, id, name, passwordHash string, policyVersion int, acceptedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET name=?,password_hash=?,status='pending',rejection_reason='',policy_version=?,policy_accepted_at_ms=?,updated_at_ms=? WHERE id=? AND status='rejected'`, name, passwordHash, policyVersion, timeMS(acceptedAt), nowMS(), id)
	return err
}
func (s *Store) SetUserRole(ctx context.Context, id string, role domain.Role) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role=?,updated_at_ms=? WHERE id=? AND status<>'deleted'`, role, nowMS(), id)
	return err
}

// DemoteAdmin atomically preserves at least one approved administrator. The
// conditional count prevents concurrent demotions from removing every active
// administrator after both requests observe the same stale count.
func (s *Store) DemoteAdmin(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE users SET role='user',updated_at_ms=?
		WHERE id=? AND role='admin' AND status<>'deleted'
		AND (status<>'approved' OR (SELECT COUNT(*) FROM users WHERE role='admin' AND status='approved')>1)`, nowMS(), id)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}
func (s *Store) SetPassword(ctx context.Context, id, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at_ms=? WHERE id=?`, hash, nowMS(), id)
	return err
}
func (s *Store) TouchLogin(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at_ms=?,updated_at_ms=? WHERE id=?`, nowMS(), nowMS(), id)
	return err
}
func (s *Store) SetIssueCooldown(ctx context.Context, id string, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET issue_cooldown_until_ms=?,updated_at_ms=? WHERE id=?`, timeMS(until), nowMS(), id)
	return err
}
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND status='approved'`).Scan(&n)
	return n, err
}

func (s *Store) AnonymizeUser(ctx context.Context, id, anon string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE audit_logs SET actor_user_id=NULL,actor_label=? WHERE actor_user_id=?`, anon, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE audit_logs SET target_user_id=NULL,target_label=? WHERE target_user_id=?`, anon, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET phone=NULL,name=?,password_hash=NULL,status='deleted',rejection_reason='',suspension_reason='',deleted_at_ms=?,updated_at_ms=? WHERE id=?`, anon, nowMS(), nowMS(), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func scanKey(scanner interface{ Scan(...any) error }) (domain.APIKey, error) {
	var k domain.APIKey
	var issued, revoked, last int64
	err := scanner.Scan(&k.ID, &k.UserID, &k.Hash, &k.LastFour, &k.Alias, &k.Status, &issued, &revoked, &last)
	k.IssuedAt = fromMS(issued)
	k.RevokedAt = fromMS(revoked)
	k.LastSeenAt = fromMS(last)
	return k, err
}

const keyColumns = `id,user_id,key_hash,last_four,alias,status,issued_at_ms,revoked_at_ms,last_seen_at_ms`

func (s *Store) ActiveKey(ctx context.Context, userID string) (domain.APIKey, error) {
	return scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE user_id=? AND status='active' ORDER BY issued_at_ms DESC LIMIT 1`, userID))
}
func (s *Store) KeyByHash(ctx context.Context, hash string) (domain.APIKey, error) {
	return scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE key_hash=?`, hash))
}
func (s *Store) ListKeysByUser(ctx context.Context, userID string) ([]domain.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE user_id=? ORDER BY issued_at_ms`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.APIKey
	for rows.Next() {
		k, e := scanKey(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
func (s *Store) CreateKey(ctx context.Context, k domain.APIKey) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(id,user_id,key_hash,last_four,alias,status,issued_at_ms) VALUES(?,?,?,?,?,?,?)`, k.ID, k.UserID, k.Hash, k.LastFour, k.Alias, k.Status, timeMS(k.IssuedAt))
	return err
}
func (s *Store) SetKeyStatus(ctx context.Context, hash, status string) error {
	revoked := int64(0)
	if status != "active" {
		revoked = nowMS()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET status=?,revoked_at_ms=? WHERE key_hash=?`, status, revoked, hash)
	return err
}
func (s *Store) SetKeyLastSeen(ctx context.Context, hash string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_seen_at_ms=? WHERE key_hash=?`, timeMS(t), hash)
	return err
}

func (s *Store) UpsertModelTestResult(ctx context.Context, result domain.ModelTestResult) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_test_results(user_id,model,status,message,latency_ms,tested_at_ms)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(user_id,model) DO UPDATE SET
		status=excluded.status,
		message=excluded.message,
		latency_ms=excluded.latency_ms,
		tested_at_ms=excluded.tested_at_ms`, result.UserID, result.Model, result.Status, result.Message, result.LatencyMS, timeMS(result.TestedAt))
	return err
}

func (s *Store) ListModelTestResults(ctx context.Context, userID string) ([]domain.ModelTestResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,model,status,message,latency_ms,tested_at_ms
		FROM model_test_results WHERE user_id=? ORDER BY model COLLATE NOCASE`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModelTestResult
	for rows.Next() {
		var result domain.ModelTestResult
		var testedAt int64
		if err := rows.Scan(&result.UserID, &result.Model, &result.Status, &result.Message, &result.LatencyMS, &testedAt); err != nil {
			return nil, err
		}
		result.TestedAt = fromMS(testedAt)
		out = append(out, result)
	}
	return out, rows.Err()
}

func (s *Store) DeleteModelTestResults(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_test_results WHERE user_id=?`, userID)
	return err
}
func (s *Store) PortalKeyHashes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_hash FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) ActiveKeys(ctx context.Context) ([]domain.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE status='active' ORDER BY issued_at_ms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.APIKey
	for rows.Next() {
		k, e := scanKey(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) SwapActiveKey(ctx context.Context, oldHash string, next domain.APIKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if oldHash != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET status='revoked',revoked_at_ms=? WHERE key_hash=? AND status='active'`, nowMS(), oldHash); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO api_keys(id,user_id,key_hash,last_four,alias,status,issued_at_ms) VALUES(?,?,?,?,?,'active',?)`, next.ID, next.UserID, next.Hash, next.LastFour, next.Alias, timeMS(next.IssuedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateSession(ctx context.Context, v Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,created_at_ms,last_seen_at_ms,expires_at_ms,ip,user_agent) VALUES(?,?,?,?,?,?,?)`, v.TokenHash, v.UserID, timeMS(v.CreatedAt), timeMS(v.LastSeen), timeMS(v.ExpiresAt), v.IP, v.UserAgent)
	return err
}
func (s *Store) Session(ctx context.Context, hash string) (Session, error) {
	var v Session
	var created, last, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT token_hash,user_id,created_at_ms,last_seen_at_ms,expires_at_ms,ip,user_agent FROM sessions WHERE token_hash=?`, hash).Scan(&v.TokenHash, &v.UserID, &created, &last, &expires, &v.IP, &v.UserAgent)
	v.CreatedAt = fromMS(created)
	v.LastSeen = fromMS(last)
	v.ExpiresAt = fromMS(expires)
	return v, err
}
func (s *Store) TouchSession(ctx context.Context, hash string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at_ms=? WHERE token_hash=?`, timeMS(t), hash)
	return err
}
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, hash)
	return err
}
func (s *Store) DeleteUserSessions(ctx context.Context, userID, exceptHash string) error {
	q := `DELETE FROM sessions WHERE user_id=?`
	args := []any{userID}
	if exceptHash != "" {
		q += ` AND token_hash<>?`
		args = append(args, exceptHash)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}
func (s *Store) CleanupSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at_ms<?`, nowMS())
	return err
}

func (s *Store) CreatePasswordReset(ctx context.Context, v PasswordReset) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id=?`, v.UserID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO password_resets(code_hash,user_id,expires_at_ms,created_at_ms) VALUES(?,?,?,?)`, v.CodeHash, v.UserID, timeMS(v.ExpiresAt), timeMS(v.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) PasswordReset(ctx context.Context, hash string) (PasswordReset, error) {
	var v PasswordReset
	var expires, used, created int64
	err := s.db.QueryRowContext(ctx, `SELECT code_hash,user_id,expires_at_ms,used_at_ms,created_at_ms FROM password_resets WHERE code_hash=?`, hash).Scan(&v.CodeHash, &v.UserID, &expires, &used, &created)
	v.ExpiresAt = fromMS(expires)
	v.UsedAt = fromMS(used)
	v.CreatedAt = fromMS(created)
	return v, err
}
func (s *Store) UsePasswordReset(ctx context.Context, hash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE password_resets SET used_at_ms=? WHERE code_hash=? AND used_at_ms=0 AND expires_at_ms>=?`, nowMS(), hash, nowMS())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Audit(ctx context.Context, e domain.AuditEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,actor_label,action,target_user_id,target_label,detail,ip,created_at_ms) VALUES(NULLIF(?,''),?,?,NULLIF(?,''),?,?,?,?)`, e.ActorUserID, e.ActorLabel, e.Action, e.TargetID, e.TargetLabel, e.Detail, e.IP, timeMS(e.CreatedAt))
	return err
}
func (s *Store) ListAudit(ctx context.Context, limit, offset int) ([]domain.AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(actor_user_id,''),actor_label,action,COALESCE(target_user_id,''),target_label,detail,ip,created_at_ms FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanAuditRows(rows)
}
func (s *Store) ListAuditForUser(ctx context.Context, userID string, limit, offset int) ([]domain.AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(actor_user_id,''),actor_label,action,COALESCE(target_user_id,''),target_label,detail,ip,created_at_ms FROM audit_logs WHERE actor_user_id=? OR target_user_id=? ORDER BY id DESC LIMIT ? OFFSET ?`, userID, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanAuditRows(rows)
}
func scanAuditRows(rows *sql.Rows) ([]domain.AuditEvent, error) {
	defer rows.Close()
	var out []domain.AuditEvent
	for rows.Next() {
		var e domain.AuditEvent
		var created int64
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.ActorLabel, &e.Action, &e.TargetID, &e.TargetLabel, &e.Detail, &e.IP, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = fromMS(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) EnqueueJob(ctx context.Context, j domain.SyncJob) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_jobs(user_id,action,key_hash,next_at_ms,created_at_ms) VALUES(?,?,?,?,?) ON CONFLICT(user_id,action,key_hash) DO UPDATE SET next_at_ms=excluded.next_at_ms`, j.UserID, j.Action, j.KeyHash, timeMS(j.NextAt), timeMS(j.CreatedAt))
	return err
}
func (s *Store) DueJobs(ctx context.Context, limit int) ([]domain.SyncJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,action,key_hash,attempts,next_at_ms,last_error,created_at_ms FROM sync_jobs WHERE next_at_ms<=? ORDER BY next_at_ms LIMIT ?`, nowMS(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SyncJob
	for rows.Next() {
		var j domain.SyncJob
		var next, created int64
		if err := rows.Scan(&j.ID, &j.UserID, &j.Action, &j.KeyHash, &j.Attempts, &next, &j.LastError, &created); err != nil {
			return nil, err
		}
		j.NextAt = fromMS(next)
		j.CreatedAt = fromMS(created)
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Store) RetryJob(ctx context.Context, id int64, next time.Time, msg string) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sync_jobs SET attempts=attempts+1,next_at_ms=?,last_error=? WHERE id=?`, timeMS(next), msg, id)
	return err
}
func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sync_jobs WHERE id=?`, id)
	return err
}

func IsUniqueError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
