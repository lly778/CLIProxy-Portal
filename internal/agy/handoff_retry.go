package agy

import (
	"net/http"
	"time"
)

func (m *Manager) handoffRequestKey(owner string, h http.Header, body []byte) string {
	key := responseReplayKey(owner, h, body)
	if key == "" {
		return ""
	}
	if m.disk != nil {
		return m.disk.hash("handoff-request:" + key)
	}
	return digest([]string{"handoff-request", m.cfg.Secret, key})
}

func hasRequestHandoff(s *session, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.pending {
		if c.requestKey == key && c.Delivered && !c.Returned && !c.Replayed {
			return true
		}
	}
	return false
}

// Resolve only the exact original owner-scoped request, including its canonical
// payload. A header alone can never select a recovery or authorize a tool action.
func (m *Manager) handoffRetrySession(owner, key string) (*session, error) {
	if key == "" {
		return nil, nil
	}
	m.mu.Lock()
	live := map[string]bool{}
	var all []*session
	for _, s := range m.sessions {
		live[s.ID] = true
		if s.owner == owner {
			all = append(all, s)
		}
	}
	m.mu.Unlock()
	var found *session
	accept := func(s *session) error {
		if !hasRequestHandoff(s, key) {
			return nil
		}
		if found != nil && found.ID != s.ID {
			return fail(409, "ambiguous_session", "原请求对应多个工具交接，未自动选择或重复执行")
		}
		found = s
		return nil
	}
	for _, s := range all {
		if err := accept(s); err != nil {
			return nil, err
		}
	}
	if m.disk == nil {
		return found, nil
	}
	d := m.disk
	cutoff := time.Now().Add(-sessionRetention).UnixMilli()
	rows, err := d.db.Query(`SELECT s.id FROM session_handoff_routes r JOIN sessions s ON s.id=r.session_id WHERE r.owner=? AND r.request_key=? AND s.ready=1 AND s.last_ms>=?`, d.hash(owner), key, cutoff)
	if err != nil {
		return nil, fail(503, "session_cache_unavailable", "AGY 工具交接索引暂不可用")
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		if !live[id] {
			ids = append(ids, id)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, fail(503, "session_cache_unavailable", "AGY 工具交接索引暂不可用")
	}
	for _, id := range ids {
		s, err := d.loadSession(id, owner, cutoff)
		if err != nil {
			return nil, fail(503, "session_cache_unavailable", "AGY 工具交接记录暂不可用")
		}
		if s != nil {
			if err := accept(s); err != nil {
				return nil, err
			}
		}
	}
	return found, nil
}
