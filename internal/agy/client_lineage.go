package agy

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

const clientBindingLimit = 8

// Existing identity headers are routing hints, never authorization or an ACK.
// Do not use turn/request IDs: they change on every turn of the same chat.
func (m *Manager) clientConversationKey(h http.Header) string {
	identity := ""
	for _, name := range []string{"Conversation_id", "Session_id"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" && len(v) <= 256 {
			identity = name + ":" + v
			break
		}
	}
	if identity == "" {
		var metadata map[string]any
		if v := h.Get("X-Codex-Turn-Metadata"); len(v) <= 4096 && unmarshalExact([]byte(v), &metadata) == nil {
			for _, name := range []string{"thread_id", "session_id"} {
				if v, ok := metadata[name].(string); ok && v != "" && len(v) <= 256 {
					identity = name + ":" + v
					break
				}
			}
		}
	}
	if identity == "" {
		return ""
	}
	if m.disk != nil {
		return m.disk.hash("client-conversation:" + identity)
	}
	return digest([]string{"client-conversation", m.cfg.Secret, identity})
}

func clientBindingAllows(s *session, r request) bool {
	if carriesRecoveredResult(s, r.Items) {
		return true // Owner-scoped call identity; acceptance is validated later.
	}
	// An explicit owned response chain can establish a new header alias.
	if r.Previous != "" || r.clientKey == "" || len(s.clientBindings) == 0 || slices.Contains(s.clientBindings, r.clientKey) {
		return true
	}
	// An original pending call also identifies its owner-scoped continuation.
	// The entire result batch must still pass the normal immutable-result checks.
	for _, item := range r.Items {
		if isToolResultItem(stringField(item, "type")) && s.pending[resultCallID(item)] != nil {
			return true
		}
	}
	return false
}

func bindClientConversation(s *session, r request) {
	if r.clientKey == "" || slices.Contains(s.clientBindings, r.clientKey) {
		return
	}
	if len(s.clientBindings) == clientBindingLimit {
		s.clientBindings = append([]string(nil), s.clientBindings[1:]...)
	}
	s.clientBindings = append(s.clientBindings, r.clientKey)
}

// Prefer this client's validated lineage over identical histories in another
// chat. An identity alone never accepts edited messages, calls or results.
// Multiple native IDs are retained across context rebuilds/compaction branches.
func (m *Manager) findClientConversation(owner string, r request) (*session, error) {
	if r.clientKey == "" || r.Previous != "" {
		return nil, nil
	}
	var found *session
	conflictingHandoff := false
	consider := func(s *session) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.owner != owner || s.model != r.Model || !slices.Contains(s.clientBindings, r.clientKey) {
			return nil
		}
		match := sessionMatchesRequest(s, r)
		if !match && s.running && !isCodexCompactionRequest(r) && prefix(s.history, requestFingerprints(r.Items)) && len(r.Items) > len(s.history) && onlyFollowupMessages(r.Items[len(s.history):]) {
			system, err := systemText(r)
			match = err == nil && s.system == digest(system)
		}
		if !match {
			for _, c := range s.pending {
				conflictingHandoff = conflictingHandoff || c.Delivered && !c.Returned && !c.Replayed
			}
			return nil
		}
		if s.loading {
			return fail(409, "session_busy", "原 AGY 会话正在恢复，请等待当前续接完成")
		}
		if found != nil && found.ID != s.ID {
			return fail(409, "ambiguous_session", "当前客户端对应多个有效会话，未自动合并")
		}
		found = s
		return nil
	}

	m.mu.Lock()
	live := map[string]bool{}
	var candidates []*session
	for _, s := range m.sessions {
		live[s.ID] = true
		candidates = append(candidates, s)
	}
	m.mu.Unlock()
	for _, s := range candidates {
		if err := consider(s); err != nil {
			return nil, err
		}
	}
	if m.disk == nil {
		if found == nil && conflictingHandoff {
			return nil, fail(409, "conversation_history_conflict", "原会话仍有工具交接，当前历史无法安全对齐，未另建会话或重复执行")
		}
		return found, nil
	}
	d := m.disk
	cutoff := time.Now().Add(-sessionRetention).UnixMilli()
	cursor := ""
	for {
		rows, err := d.db.Query(`SELECT s.id FROM session_client_routes r JOIN sessions s ON s.id=r.session_id WHERE r.owner=? AND r.model=? AND r.client_key=? AND s.ready=1 AND s.last_ms>=? AND s.id>? ORDER BY s.id LIMIT 32`, d.hash(owner), d.hash(r.Model), r.clientKey, cutoff, cursor)
		if err != nil {
			return nil, fail(503, "session_cache_unavailable", "AGY 客户端会话索引暂不可用")
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return nil, fail(503, "session_cache_unavailable", "AGY 客户端会话索引暂不可用")
		}
		for _, id := range ids {
			if live[id] {
				continue
			}
			s, err := d.loadSession(id, owner, cutoff)
			if err != nil {
				return nil, fail(503, "session_cache_unavailable", "AGY 客户端会话记录暂不可用")
			}
			if s != nil {
				if err := consider(s); err != nil {
					return nil, err
				}
			}
		}
		if len(ids) < 32 {
			break
		}
		cursor = ids[len(ids)-1]
	}
	if found == nil && conflictingHandoff {
		return nil, fail(409, "conversation_history_conflict", "原会话仍有工具交接，当前历史无法安全对齐，未另建会话或重复执行")
	}
	return found, nil
}

func (m *Manager) find(owner string, r request) (*session, error) {
	if s, err := m.findClientConversation(owner, r); err != nil || s != nil {
		return s, err
	}
	return m.findHistory(owner, r)
}
