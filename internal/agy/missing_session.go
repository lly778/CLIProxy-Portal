package agy

import (
	"context"
	"errors"
	"maps"
	"strings"
)

func explicitMissingSession(e *rpcError) bool {
	if e.Code != -32602 && e.Code != -32001 && e.Code != -32603 {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(e.Message))
	return message == "session not found" || message == "unknown session" || strings.HasPrefix(message, "session not found: ") || strings.HasPrefix(message, "unknown session: ")
}

func canRebuildMissingSession(s *session, r request) bool {
	if isCodexCompactionRequest(r) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.persisted || s.interrupted || s.running || !s.waitAt.IsZero() || len(s.pending) != 0 || len(s.items) == 0 || s.model != r.Model {
		return false
	}
	system, err := systemText(r)
	if err != nil || s.system != digest(system) || len(r.Items) < len(s.items) || !prefix(s.history, requestFingerprints(r.Items)) {
		return false
	}
	if !onlyFollowupMessages(r.Items[len(s.items):]) {
		return false
	}
	history, err := inspectToolHistory(r.Items)
	return err == nil && len(history.Open) == 0
}

func (m *Manager) rebuildMissingSession(ctx context.Context, old *session, r request) (*session, error) {
	if !canRebuildMissingSession(old, r) {
		return nil, fail(409, "session_restore_unsafe", "原生会话不存在，当前工具或历史状态不允许自动重建，原记录已保留")
	}
	release, err := m.claimSessionRecovery(old)
	if err != nil {
		return nil, err
	}
	defer release()
	old.mu.Lock()
	tools, loaded := requestedTools(old, r)
	deferred := old.deferredTools
	if r.ToolsProvided {
		deferred = deferredCatalog(r.Tools)
	}
	responses := maps.Clone(old.responses)
	old.mu.Unlock()
	r.Tools, r.ToolsProvided = mergeTools(mergeTools(tools, loaded), deferred), true
	next, err := m.create(ctx, old.owner, r)
	if err != nil {
		return nil, err
	}
	if m.disk != nil {
		if _, err = m.disk.db.ExecContext(ctx, `UPDATE sessions SET ready=0 WHERE id=?`, old.ID); err != nil {
			m.drop(next)
			return nil, fail(503, "session_cache_unavailable", "恢复索引无法更新，原记录已保留")
		}
	}
	next.mu.Lock()
	next.responses = responses
	next.mu.Unlock()
	traceFrom(ctx).emit(Observation{Kind: "session", State: "rebuild", SessionID: next.ID, Reason: "confirmed_missing_session"})
	return next, nil
}

func isMissingSession(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "native_session_missing"
}
