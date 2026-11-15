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

// Normal retries still use the existing response cache. If that cache was lost
// in a crash but an original tool handoff survived, do not start the original
// prompt again. Result continuations and new user input have different keys.
// No failed-request tombstone is created; this guard exists only as long as
// the original waiting call exists and never implies that the action ran.
func (m *Manager) guardHandoffRetry(owner, key string) error {
	if key == "" {
		return nil
	}
	blocked := func() error {
		return fail(409, "tool_handoff_pending", "原工具交接记录已保留，请回传原调用结果或按最新历史续接，未重复执行原请求")
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
	for _, s := range all {
		if hasRequestHandoff(s, key) {
			return blocked()
		}
	}
	if m.disk == nil {
		return nil
	}
	d := m.disk
	cutoff := time.Now().Add(-sessionRetention).UnixMilli()
	// Metadata lookup only: no scan or decryption of conversation bodies.
	rows, err := d.db.Query(`SELECT s.id FROM session_handoff_routes r JOIN sessions s ON s.id=r.session_id WHERE r.owner=? AND r.request_key=? AND s.ready=1 AND s.last_ms>=?`, d.hash(owner), key, cutoff)
	if err != nil {
		return fail(503, "session_cache_unavailable", "AGY 工具交接索引暂不可用")
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return fail(503, "session_cache_unavailable", "AGY 工具交接索引暂不可用")
		}
		if !live[id] {
			return blocked()
		}
	}
	if rows.Err() != nil {
		return fail(503, "session_cache_unavailable", "AGY 工具交接索引暂不可用")
	}
	return nil
}
