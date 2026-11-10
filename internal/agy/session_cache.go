package agy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const sessionRetention = 7 * 24 * time.Hour

// Only completed, idle sessions are recoverable. Pending tools and partial turns
// are never replayed after a process restart. This encrypted index also retains
// authoritative wire history so an explicit instruction/history change can be
// rebuilt without re-executing tools. The official session remains in AGY's DB.
type savedSession struct {
	OfficialToolsVersion                       int
	ID, Owner, Model, System, ToolHash, Effort string
	Tools                                      []tool
	AdvertisedTools, LoadedTools               []tool
	History                                    []string
	Items                                      []map[string]any
	Responses                                  map[string]bool
	Last                                       time.Time
}

type sessionDiskCache struct {
	db    *sql.DB
	crypt cipher.AEAD
	key   []byte
	count atomic.Int64
	mu    sync.Mutex
}

func openSessionDiskCache(home, secret string) (*sessionDiskCache, error) {
	path := filepath.Join(home, "portal-sessions.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	key := sha256.Sum256([]byte("cliproxy-portal/agy/session-cache/v1\x00" + secret))
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(block)
	d := &sessionDiskCache{db: db, crypt: aead, key: key[:]}
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=3000", "PRAGMA synchronous=NORMAL", "PRAGMA cache_size=-2048", "PRAGMA mmap_size=0",
		`CREATE TABLE IF NOT EXISTS cache_meta (id INTEGER PRIMARY KEY, value BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, owner TEXT NOT NULL, model TEXT NOT NULL, history_key TEXT NOT NULL, last_ms INTEGER NOT NULL, ready INTEGER NOT NULL, payload BLOB NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS sessions_history ON sessions(owner,model,history_key)`,
		`CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(last_ms)`,
		`CREATE TABLE IF NOT EXISTS responses (owner TEXT NOT NULL, response TEXT NOT NULL, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, PRIMARY KEY(owner,response))`,
		`CREATE INDEX IF NOT EXISTS responses_session ON responses(session_id)`,
		`CREATE TABLE IF NOT EXISTS prompt_attempts (key TEXT PRIMARY KEY, state TEXT NOT NULL, at_ms INTEGER NOT NULL)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	// A prior process cannot still be executing these requests. Preserve the
	// conservative unknown outcome; only pre-submit reservations are retryable.
	if _, err = db.Exec(`DELETE FROM prompt_attempts WHERE state='reserved'`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err = db.Exec(`UPDATE prompt_attempts SET state='unknown' WHERE state='parked'`); err != nil {
		_ = db.Close()
		return nil, err
	}
	var marker []byte
	err = db.QueryRow(`SELECT value FROM cache_meta WHERE id=1`).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		marker, err = d.seal("cache-key", []byte("portal-session-cache-v1"))
		if err == nil {
			_, err = db.Exec(`INSERT INTO cache_meta(id,value) VALUES(1,?)`, marker)
		}
	} else if err == nil {
		var plain []byte
		plain, err = d.open("cache-key", marker)
		if err == nil && string(plain) != "portal-session-cache-v1" {
			err = errors.New("invalid AGY session cache key")
		}
	}
	if err == nil {
		var count int64
		err = db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count)
		d.count.Store(count)
	}
	if err != nil {
		_ = db.Close()
		return nil, errors.New("AGY session cache cannot be opened or decrypted")
	}
	return d, nil
}

func (d *sessionDiskCache) seal(id string, plain []byte) ([]byte, error) {
	nonce := make([]byte, d.crypt.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return d.crypt.Seal(nonce, nonce, plain, []byte(id)), nil
}
func (d *sessionDiskCache) open(id string, sealed []byte) ([]byte, error) {
	n := d.crypt.NonceSize()
	if len(sealed) < n+d.crypt.Overhead() {
		return nil, errors.New("invalid AGY session cache")
	}
	return d.crypt.Open(nil, sealed[:n], sealed[n:], []byte(id))
}
func (d *sessionDiskCache) hash(v string) string {
	h := hmac.New(sha256.New, d.key)
	_, _ = h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil))
}
func (d *sessionDiskCache) historyKeys(history []string) []string {
	h := hmac.New(sha256.New, d.key)
	keys := make([]string, 0, len(history))
	for _, fp := range history {
		_, _ = h.Write([]byte(fp + "\n"))
		keys = append(keys, hex.EncodeToString(h.Sum(nil)))
	}
	return keys
}
func snapshotSession(s *session) savedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	responses := map[string]bool{}
	for id, usable := range s.responses {
		responses[id] = usable
	}
	return savedSession{OfficialToolsVersion: s.officialVersion, ID: s.ID, Owner: s.owner, Model: s.model, System: s.system, ToolHash: s.toolHash, Effort: s.effort, Tools: s.tools, AdvertisedTools: s.advertisedTools, LoadedTools: s.loadedTools, History: append([]string(nil), s.history...), Items: append([]map[string]any(nil), s.items...), Responses: responses, Last: s.last}
}
func (d *sessionDiskCache) put(v savedSession, ready bool) error {
	if !safeNativeSessionID(v.ID) || v.Owner == "" {
		return errors.New("invalid AGY session state")
	}
	plain, err := json.Marshal(v)
	if err != nil || len(plain) > MaxBody*2 {
		return errors.New("AGY session routing state is too large")
	}
	payload, err := d.seal(v.ID, plain)
	if err != nil {
		return err
	}
	keys := d.historyKeys(v.History)
	lastKey := ""
	if len(keys) > 0 {
		lastKey = keys[len(keys)-1]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT OR IGNORE INTO sessions(id,owner,model,history_key,last_ms,ready,payload) VALUES(?,?,?,?,?,?,?)`, v.ID, d.hash(v.Owner), d.hash(v.Model), lastKey, v.Last.UnixMilli(), ready, payload)
	if err != nil {
		return err
	}
	inserted, _ := r.RowsAffected()
	if inserted == 0 {
		_, err = tx.Exec(`UPDATE sessions SET owner=?,model=?,history_key=?,last_ms=?,ready=?,payload=? WHERE id=?`, d.hash(v.Owner), d.hash(v.Model), lastKey, v.Last.UnixMilli(), ready, payload, v.ID)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`DELETE FROM responses WHERE session_id=?`, v.ID); err != nil {
		return err
	}
	if ready {
		for id, usable := range v.Responses {
			if usable {
				if _, err = tx.Exec(`INSERT INTO responses(owner,response,session_id) VALUES(?,?,?)`, d.hash(v.Owner), d.hash(id), v.ID); err != nil {
					return err
				}
			}
		}
	}
	if err = tx.Commit(); err == nil {
		d.count.Add(inserted)
	}
	return err
}

// Candidate lookup is indexed and bounded in batches. No archive-wide metadata
// scan or in-memory map grows with the number of disk sessions.
func (d *sessionDiskCache) find(owner string, r request, live map[string]bool) (*session, error) {
	var ids []string
	cutoff := time.Now().Add(-sessionRetention).UnixMilli()
	if r.Previous != "" {
		var id string
		err := d.db.QueryRow(`SELECT s.id FROM responses r JOIN sessions s ON s.id=r.session_id WHERE r.owner=? AND r.response=? AND s.ready=1 AND s.last_ms>=?`, d.hash(owner), d.hash(r.Previous), cutoff).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	} else {
		keys := d.historyKeys(requestFingerprints(r.Items))
		for len(keys) > 0 {
			n := min(128, len(keys))
			args := []any{d.hash(owner), d.hash(r.Model), cutoff}
			for _, key := range keys[:n] {
				args = append(args, key)
			}
			q := `SELECT id FROM sessions WHERE owner=? AND model=? AND ready=1 AND last_ms>=? AND history_key IN (` + strings.TrimSuffix(strings.Repeat("?,", n), ",") + `)`
			rows, err := d.db.Query(q, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if !live[id] {
					ids = append(ids, id)
				}
				if len(ids) > 1 {
					break
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
			if len(ids) > 1 {
				return nil, fail(409, "ambiguous_session", "无法确认 AGY 会话，请新建对话")
			}
			keys = keys[n:]
		}
	}
	var found *session
	for _, id := range ids {
		if live[id] {
			continue
		}
		var payload []byte
		if err := d.db.QueryRow(`SELECT payload FROM sessions WHERE id=? AND ready=1 AND last_ms>=?`, id, cutoff).Scan(&payload); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		plain, err := d.open(id, payload)
		if err != nil {
			return nil, err
		}
		var v savedSession
		if json.Unmarshal(plain, &v) != nil || v.ID != id || v.Owner != owner {
			return nil, errors.New("invalid AGY session routing state")
		}
		if found != nil {
			return nil, fail(409, "ambiguous_session", "无法确认 AGY 会话，请新建对话")
		}
		found = &session{ID: v.ID, owner: v.Owner, model: v.Model, system: v.System, toolHash: v.ToolHash, effort: v.Effort, tools: v.Tools, history: v.History, items: v.Items, responses: v.Responses, last: v.Last, persisted: true}
		found.advertisedTools, found.loadedTools = v.AdvertisedTools, v.LoadedTools
		found.officialVersion = v.OfficialToolsVersion
		found.tools = filterClientTools(found.tools)
		found.advertisedTools = filterClientTools(found.advertisedTools)
		found.loadedTools = filterClientTools(found.loadedTools)
		if found.advertisedTools == nil {
			found.advertisedTools = append([]tool{}, v.Tools...)
		}
	}
	return found, nil
}

func (m *Manager) saveSession(s *session, ready bool) error {
	if m.disk == nil || s.internal {
		return nil
	}
	err := m.disk.put(snapshotSession(s), ready)
	m.mu.Lock()
	m.cacheError = err != nil
	m.mu.Unlock()
	if err != nil {
		return fail(503, "session_cache_unavailable", "AGY 会话缓存暂不可用，当前上下文未移入硬盘")
	}
	s.mu.Lock()
	s.persisted = ready
	s.mu.Unlock()
	return nil
}

func (m *Manager) archiveSession(ctx context.Context, s *session) error {
	s.mu.Lock()
	idle := !s.running && s.waitAt.IsZero() && !s.loading
	hasHistory := len(s.history) > 0
	s.mu.Unlock()
	if !idle {
		return fail(409, "session_busy", "AGY 会话仍在运行，不能转存硬盘")
	}
	if m.disk != nil && !s.internal && hasHistory && s.proc.canLoad {
		if err := m.saveSession(s, true); err != nil {
			return err
		}
	}
	if s.ID != "" && s.proc != nil && s.proc.alive() {
		if err := s.proc.call(ctx, "session/close", map[string]string{"sessionId": s.ID}, nil); err != nil {
			return err
		}
	}
	s.cancel()
	return nil
}

func (m *Manager) restoreSession(ctx context.Context, s *session, r request) (*session, error) {
	system, err := systemText(r)
	if err != nil {
		return nil, err
	}
	if s.model != r.Model || s.system != digest(system) {
		return nil, fail(409, "session_changed", "AGY 会话的模型或系统指令发生变化，请新建对话")
	}
	p, err := m.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if !p.canLoad {
		return nil, fail(503, "session_restore_unsupported", "当前 AGY 版本不支持恢复硬盘会话")
	}
	s.life, s.cancel = context.WithCancel(m.life)
	s.tenant, s.secret = newID("s_"), newID("token_")
	s.pending, s.reported = map[string]*pendingTool{}, map[string]reportedTool{}
	s.events = make(chan agentEvent, 256)
	s.proc = p
	s.loading = true
	victim, ok := m.reserveSession(s)
	if !ok {
		s.cancel()
		return nil, fail(429, "session_limit", "AGY 会话已满，请先完成当前交互")
	}
	if victim != nil {
		err = m.archiveSession(ctx, victim)
		victim.requestMu.Unlock()
		if err != nil {
			m.drop(s)
			m.mu.Lock()
			m.sessions[victim.tenant] = victim
			m.mu.Unlock()
			return nil, err
		}
	}
	params := map[string]any{"sessionId": s.ID, "cwd": filepath.Join(m.cfg.Home, "workspace"), "mcpServers": []any{map[string]any{"type": "http", "name": bridgeName, "url": m.mcpURL + s.tenant, "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + s.secret}}}}}
	c, done := shortContext(ctx)
	defer done()
	var n newSession
	if err = p.call(c, "session/load", params, &n); err != nil {
		m.drop(s)
		return nil, fail(503, "session_restore_failed", "AGY 硬盘会话恢复失败，原缓存已保留")
	}
	s.mu.Lock()
	s.options = n.Options
	s.loading = false
	s.mu.Unlock()
	if err = m.selectOptions(c, s, r); err != nil {
		m.drop(s)
		return nil, err
	}
	return s, nil
}

func (m *Manager) pruneSessionCache(now time.Time) error {
	if m.disk == nil {
		return nil
	}
	// Serialize pruning with generations and catalog loads; no in-flight turn or
	// tool result may be removed even if the wall clock jumps forward.
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	default:
		return nil
	}
	m.mu.Lock()
	live := map[string]bool{}
	for _, s := range m.sessions {
		live[s.ID] = true
	}
	m.mu.Unlock()
	rows, err := m.disk.db.Query(`SELECT id FROM sessions WHERE last_ms<? LIMIT 128`, now.Add(-sessionRetention).UnixMilli())
	if err != nil {
		return err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		if !live[id] {
			expired = append(expired, id)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range expired {
		if !safeNativeSessionID(id) {
			continue
		}
		base := filepath.Join(m.cfg.Home, "agy", "conversations")
		homeReal, e := filepath.EvalSymlinks(m.cfg.Home)
		if e != nil {
			return e
		}
		baseReal, e := filepath.EvalSymlinks(base)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && baseReal != filepath.Join(homeReal, "agy", "conversations") {
			return errors.New("AGY native cache directory redirects outside its home")
		}
		for _, suffix := range []string{".db-wal", ".db-shm", ".db"} {
			path := filepath.Join(base, id+suffix)
			info, e := os.Lstat(path)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return errors.New("invalid AGY native cache file")
			}
			if e = os.Remove(path); e != nil {
				return e
			}
		}
		// Generated images and official transcripts have the same seven-day
		// owner/session retention as their routing index and native database.
		if e = removeOfficialArtifacts(m.cfg.Home, id); e != nil {
			return e
		}
		r, e := m.disk.db.Exec(`DELETE FROM sessions WHERE id=? AND last_ms<?`, id, now.Add(-sessionRetention).UnixMilli())
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		m.disk.count.Add(-n)
	}
	return nil
}
func safeNativeSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
