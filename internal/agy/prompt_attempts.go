package agy

import (
	"database/sql"
	"errors"
	"time"
)

const attemptRetention = 6 * time.Hour

type promptAttempt struct {
	State string
	At    time.Time
}

// Records contain hashes only. Mark before releasing a tool or writing to ACP:
// after a crash the outcome is unknown and must not be blindly submitted again.
func (m *Manager) claimAttempt(owner string, r request) (string, error) {
	key := digest(map[string]any{"owner": owner, "model": r.Model, "previous": r.Previous, "instructions": r.Instructions, "items": requestFingerprints(r.Items), "tools": r.Tools, "tools_provided": r.ToolsProvided, "choice": r.ToolChoice, "parallel": r.Parallel, "reasoning": r.Reasoning})
	if m.disk != nil {
		key = m.disk.hash(key)
	}
	m.attemptMu.Lock()
	defer m.attemptMu.Unlock()
	now := time.Now()
	if m.attempts == nil {
		m.attempts = map[string]promptAttempt{}
	}
	for k, v := range m.attempts {
		if now.Sub(v.At) > attemptRetention {
			delete(m.attempts, k)
		}
	}
	if v, ok := m.attempts[key]; ok {
		return "", attemptError(v.State)
	}
	if m.disk != nil {
		if _, err := m.disk.db.Exec(`DELETE FROM prompt_attempts WHERE at_ms<?`, now.Add(-attemptRetention).UnixMilli()); err != nil {
			return "", attemptStoreError()
		}
		var state string
		err := m.disk.db.QueryRow(`SELECT state FROM prompt_attempts WHERE key=?`, key).Scan(&state)
		if err == nil {
			return "", attemptError(state)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", attemptStoreError()
		}
		var count int
		if err = m.disk.db.QueryRow(`SELECT COUNT(*) FROM prompt_attempts`).Scan(&count); err != nil {
			return "", attemptStoreError()
		}
		if count >= 2048 {
			return "", fail(429, "attempt_limit", "AGY 请求保护记录已满，请稍后重试")
		}
		if _, err = m.disk.db.Exec(`INSERT INTO prompt_attempts(key,state,at_ms) VALUES(?,?,?)`, key, "reserved", now.UnixMilli()); err != nil {
			return "", attemptStoreError()
		}
	} else if len(m.attempts) >= 2048 {
		return "", fail(429, "attempt_limit", "AGY 请求保护记录已满")
	}
	m.attempts[key] = promptAttempt{State: "reserved", At: now}
	return key, nil
}

func attemptError(state string) error {
	if state == "reserved" || state == "parked" {
		return fail(409, "request_in_progress", "该请求已经提交或正在等待工具结果，请继续原响应链，不要重复执行")
	}
	return fail(409, "request_outcome_unknown", "相同请求曾中断，执行结果无法确认；请先核对客户端工具结果，再明确发起新请求")
}
func attemptStoreError() error {
	return fail(503, "attempt_store_unavailable", "AGY 请求保护记录暂不可用，请稍后重试")
}

func (m *Manager) attemptState(key, state string) error {
	if key == "" {
		return nil
	}
	m.attemptMu.Lock()
	defer m.attemptMu.Unlock()
	if m.disk != nil {
		var err error
		if state == "" {
			_, err = m.disk.db.Exec(`DELETE FROM prompt_attempts WHERE key=?`, key)
		} else {
			_, err = m.disk.db.Exec(`UPDATE prompt_attempts SET state=?,at_ms=? WHERE key=?`, state, time.Now().UnixMilli(), key)
		}
		if err != nil {
			return attemptStoreError()
		}
	}
	if state == "" {
		delete(m.attempts, key)
	} else {
		if m.attempts == nil {
			m.attempts = map[string]promptAttempt{}
		}
		m.attempts[key] = promptAttempt{State: state, At: time.Now()}
	}
	return nil
}

func (m *Manager) armAttempt(s *session, key string) error {
	if err := m.attemptState(key, "unknown"); err != nil {
		return err
	}
	s.mu.Lock()
	if s.attempts == nil {
		s.attempts = map[string]bool{}
	}
	s.attempts[key] = true
	s.mu.Unlock()
	return nil
}

func (m *Manager) settleAttempts(s *session, success bool) {
	s.mu.Lock()
	keys := []string{}
	for key := range s.attempts {
		keys = append(keys, key)
	}
	s.mu.Unlock()
	state := "unknown"
	if success {
		state = ""
	}
	for _, key := range keys {
		_ = m.attemptState(key, state)
	}
	if success {
		s.mu.Lock()
		s.attempts = map[string]bool{}
		s.mu.Unlock()
	}
}
