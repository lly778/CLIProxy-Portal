package agy

import (
	"context"
	"time"
)

// Queued requests do not consume generation slots: the current turn must still
// be able to receive its tool results, even with a one-slot configuration.
const maxQueuedFollowups = 4

type queuedFollowup struct {
	base   []string
	anchor string
	input  []map[string]any
	fps    []string
}

func onlyFollowupMessages(items []map[string]any) bool {
	user := false
	for _, item := range items {
		role := stringField(item, "role")
		if role != "user" && role != "developer" && role != "system" {
			return false
		}
		user = user || role == "user"
	}
	return user
}

func followupInput(history, fps []string, items []map[string]any) (int, bool) {
	shared := 0
	for shared < len(fps) && shared < len(history) && fps[shared] == history[shared] {
		shared++
	}
	return shared, shared > 0 && onlyFollowupMessages(items[shared:])
}

// Admission for a follow-up must not need the slot currently owned by its
// original response. Actual generation still reacquires a slot after waiting.
func canQueueFollowup(s *session, r request) bool {
	if isCodexCompactionRequest(r) || r.ToolsProvided && len(r.Tools) == 0 {
		return false
	}
	system, err := systemText(r)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil || s.model != r.Model || s.system != digest(system) || len(s.history) == 0 {
		return false
	}
	_, follows := followupInput(s.history, requestFingerprints(r.Items), r.Items)
	return follows || r.Previous != "" && onlyFollowupMessages(r.Items)
}

func (m *Manager) waitUserFollowup(ctx context.Context, s *session, r *request, out *response, releaseSlot func(), takeSlot func() error) (bool, func(), error) {
	if isCodexCompactionRequest(*r) || r.ToolsProvided && len(r.Tools) == 0 {
		return false, nil, nil
	}
	system, err := systemText(*r)
	if err != nil {
		return false, nil, err
	}
	s.mu.Lock()
	if s.model != r.Model || s.system != digest(system) || len(s.history) == 0 {
		s.mu.Unlock()
		return false, nil, nil
	}
	fps := requestFingerprints(r.Items)
	shared, follows := followupInput(s.history, fps, r.Items)
	if !follows {
		s.mu.Unlock()
		return false, nil, nil
	}
	input := r.Items[shared:]
	busy := s.running || hasWaitingResults(s) || len(s.followups) > 0
	if !busy && s.requestMu.TryLock() {
		s.requestMu.Unlock()
		s.mu.Unlock()
		return false, nil, nil
	}
	limit := maxQueuedFollowups
	if r.followupLimit != nil {
		limit = *r.followupLimit
	}
	if m.queuedFollowups.Add(1) > int64(limit) {
		m.queuedFollowups.Add(-1)
		s.mu.Unlock()
		return false, nil, fail(429, "followup_queue_full", "追加消息队列已满，请等待前面的消息处理完成")
	}
	entry := &queuedFollowup{base: append([]string(nil), fps[:shared]...), anchor: digest(fps[:shared]), input: input, fps: requestFingerprints(input)}
	s.followups = append(s.followups, entry)
	s.mu.Unlock()
	remove := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, pending := range s.followups {
			if pending == entry {
				copy(s.followups[i:], s.followups[i+1:])
				s.followups[len(s.followups)-1] = nil
				s.followups = s.followups[:len(s.followups)-1]
				m.queuedFollowups.Add(-1)
				return
			}
		}
	}
	defer remove()
	releaseSlot()
	if r.Stream {
		if err := out.event("response.in_progress", map[string]any{"response": out.envelope("in_progress", Usage{}), "stage": "queued_followup"}); err != nil {
			return false, nil, err
		}
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		if !prefix(entry.base, s.history) {
			s.mu.Unlock()
			return false, nil, fail(409, "followup_history_changed", "原会话历史已变化，未执行排队消息，请按最新历史重试")
		}
		if len(s.followups) > 0 && s.followups[0] == entry && !s.running && !hasWaitingResults(s) && s.requestMu.TryLock() {
			input := entry.input
			if s.followupAnchor == entry.anchor && prefix(s.followupInputs, entry.fps) && len(s.followupInputs) < len(entry.fps) {
				input = input[len(s.followupInputs):]
			}
			r.Items = append(append([]map[string]any(nil), s.items...), input...)
			accepted := requestFingerprints(r.Items)
			s.mu.Unlock()
			if err := takeSlot(); err != nil {
				s.requestMu.Unlock()
				return false, nil, err
			}
			remove()
			return true, func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				if prefix(accepted, s.history) {
					s.followupAnchor, s.followupInputs = entry.anchor, append([]string(nil), entry.fps...)
				}
			}, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		case <-s.life.Done():
			return false, nil, fail(503, "worker_lost", "原 AGY 会话已终止，未执行排队消息")
		case <-ticker.C:
		}
	}
}
