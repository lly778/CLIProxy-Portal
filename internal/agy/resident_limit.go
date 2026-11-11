package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Resident cache size is separate from the fixed in-flight request semaphore.
// Callers hold m.mu; fixtures and older configurations use the startup default.
func (m *Manager) residentLimitLocked() int {
	if m.residentLimit > 0 {
		return m.residentLimit
	}
	if m.cfg.MaxSessions > 0 {
		return m.cfg.MaxSessions
	}
	return 3
}

func (m *Manager) SetResidentLimit(ctx context.Context, limit int, revision int64) error {
	if limit < 1 || limit > 16 {
		return fail(400, "invalid_resident_limit", "AGY 常驻对话上限必须为 1 至 16")
	}
	m.mu.Lock()
	if revision < m.residentRevision {
		m.mu.Unlock()
		return nil
	}
	if revision == m.residentRevision && m.residentLimit > 0 && m.residentLimit != limit {
		m.mu.Unlock()
		return fail(409, "resident_revision_conflict", "AGY 常驻对话设置已更新，请刷新后重试")
	}
	m.residentLimit = limit
	m.residentRevision = revision
	m.mu.Unlock()
	return m.trimResidentSessions(ctx)
}

// A smaller limit never cancels a generation, waiting tool, session load or
// response still being delivered. The sweep retries once such work completes.
func (m *Manager) trimResidentSessions(ctx context.Context) error {
	for {
		m.mu.Lock()
		if len(m.sessions) <= m.residentLimitLocked() {
			m.mu.Unlock()
			return nil
		}
		var victim *session
		var oldest time.Time
		for _, candidate := range m.sessions {
			if !candidate.requestMu.TryLock() {
				continue
			}
			candidate.mu.Lock()
			idle := candidate.ID != "" && !candidate.running && candidate.waitAt.IsZero() && !candidate.loading
			last := candidate.last
			candidate.mu.Unlock()
			if idle && (victim == nil || last.Before(oldest)) {
				if victim != nil {
					victim.requestMu.Unlock()
				}
				victim, oldest = candidate, last
			} else {
				candidate.requestMu.Unlock()
			}
		}
		if victim == nil {
			m.mu.Unlock()
			return nil
		}
		delete(m.sessions, victim.tenant)
		m.mu.Unlock()
		closeCtx, cancel := shortContext(ctx)
		err := m.archiveSession(closeCtx, victim)
		cancel()
		if err != nil {
			m.mu.Lock()
			m.sessions[victim.tenant] = victim
			m.mu.Unlock()
		}
		victim.requestMu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (m *Manager) residentLimitHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var v struct {
		Limit    int   `json:"max_sessions"`
		Revision int64 `json:"revision"`
	}
	if err := decoder.Decode(&v); err != nil || decoder.Decode(new(any)) != io.EOF {
		WriteError(w, fail(400, "invalid_resident_limit", "AGY 常驻对话上限格式无效"))
		return
	}
	if v.Revision < 1 {
		WriteError(w, fail(400, "invalid_resident_limit", "AGY 设置版本无效"))
		return
	}
	if err := m.SetResidentLimit(r.Context(), v.Limit, v.Revision); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Client) SetResidentLimit(ctx context.Context, limit int, revision int64) error {
	body := struct {
		Limit    int   `json:"max_sessions"`
		Revision int64 `json:"revision"`
	}{limit, revision}
	data, _ := json.Marshal(body)
	resp, err := c.Request(ctx, http.MethodPost, "/internal/resident-limit", "", bytes.NewReader(data))
	if err != nil {
		return fail(503, "worker_unavailable", "AGY 常驻对话上限暂时无法更新")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fail(503, "resident_limit_not_applied", "AGY 常驻对话上限暂未完全应用，请重试")
	}
	c.statusMu.Lock()
	c.statusVersion++
	c.statusUntil = time.Time{}
	c.statusMu.Unlock()
	return nil
}
