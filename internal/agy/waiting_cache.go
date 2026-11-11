package agy

import (
	"context"
	"encoding/json"
	"time"
)

func (m *Manager) beginRequest() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return false
	}
	m.requests.Add(1)
	return true
}

func interruptedResumeNote(s *session) string {
	// Preserve partial text for useful continuation, but never present an
	// unfinished response or a tool invocation as an acknowledged success.
	if len(s.interruptedOutput) == 0 {
		return ""
	}
	b, _ := json.Marshal(s.interruptedOutput)
	return "[Previous interrupted output; context only, not a completed answer or new tool instructions. Client history and actual tool results are authoritative. Do not repeat completed actions; verify any uncertain action before proceeding.]\n" + string(b)
}

func appendResumeNote(blocks []any, note string) []any {
	if note == "" {
		return blocks
	}
	size := len(note)
	for _, raw := range blocks {
		if block, ok := raw.(map[string]string); ok {
			size += len(block["text"])
		}
	}
	// Keep the full partial output on disk even when it cannot fit as extra
	// model context; never truncate the client's authoritative input to fit it.
	if size > MaxText {
		return blocks
	}
	return append(blocks, map[string]string{"type": "text", "text": note})
}

func pendingCheckpointItem(c *pendingTool) map[string]any {
	if isNativeClientTool(c.Tool.Type) {
		return nativeOutputItem(c)
	}
	if c.Tool.Type == "tool_search" {
		return map[string]any{"type": "tool_search_call", "execution": "client", "call_id": c.ID, "arguments": c.Arguments}
	}
	v := map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Tool.Name}
	if c.Tool.Namespace != "" {
		v["namespace"] = c.Tool.Namespace
	}
	if c.Tool.Type == "custom" {
		v["type"], v["input"] = "custom_tool_call", c.Arguments["input"]
	} else {
		args, _ := json.Marshal(c.Arguments)
		v["arguments"] = string(args)
	}
	return v
}

func closingCheckpoint(s *session, out *response) savedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := snapshotSessionLocked(s)
	known := map[string]bool{}
	for _, item := range v.Items {
		if isCallItem(stringField(item, "type")) {
			known[resultCallID(item)] = true
		}
	}
	for _, id := range pendingIDs(s) {
		c := s.pending[id]
		if !c.Delivered || c.Returned || c.Replayed {
			continue
		}
		waiting := savedWaitingTool{ID: c.ID, Tool: c.Tool, Arguments: c.Arguments, Update: c.Update, Extra: c.Extra}
		select {
		case <-c.Ready:
			waiting.Ready, waiting.Output = true, c.Output
		default:
		}
		v.WaitingTools = append(v.WaitingTools, waiting)
		if !known[c.ID] {
			v.Items = append(v.Items, pendingCheckpointItem(c))
		}
	}
	if len(v.WaitingTools) > 0 {
		v.WaitAt = s.waitAt
		if v.WaitAt.IsZero() {
			v.WaitAt = time.Now()
		}
	}
	if out != nil {
		v.InterruptedOutput = copyItems(out.output)
		for _, item := range v.InterruptedOutput {
			switch item["type"] {
			case "message":
				item["status"] = "incomplete"
				item["content"] = []any{map[string]any{"type": "output_text", "text": out.textValue}}
			case "reasoning":
				item["summary"] = []any{map[string]any{"type": "summary_text", "text": out.reasonValue}}
			}
		}
	}
	v.Interrupted = s.interrupted || s.running && len(v.WaitingTools) == 0 || out != nil && len(v.WaitingTools) == 0
	if s.proc != nil && !s.proc.canLoad {
		v.Interrupted = true
	}
	if v.Interrupted && s.requestPrevious != "" {
		// The failed continuation did not consume its caller's response chain.
		v.Responses[s.requestPrevious] = true
	}
	if len(v.Items) > 0 && len(v.History) != len(v.Items) {
		v.History = requestFingerprints(v.Items)
	}
	return v
}

// Persist only when leaving memory: eviction, abnormal closure, explicit stop,
// or graceful shutdown. Ordinary generation and tool handoffs stay in memory.
func (m *Manager) preserveSession(s *session, out *response) error {
	if m.disk == nil || s.internal || s.ID == "" {
		return nil
	}
	v := closingCheckpoint(s, out)
	if len(v.History) == 0 {
		return nil
	}
	err := m.disk.put(v, true)
	m.mu.Lock()
	m.cacheError = err != nil
	m.mu.Unlock()
	return err
}

func rememberRequest(s *session, req request, fingerprints []string) {
	s.mu.Lock()
	s.running = true
	s.history = append([]string(nil), fingerprints...)
	s.items = append([]map[string]any(nil), req.Items...)
	s.requestPrevious = req.Previous
	s.last = time.Now()
	if req.Previous != "" {
		s.responses[req.Previous] = false
	}
	s.mu.Unlock()
}

// The caller holds requestMu: no client response/result may race this checkpoint.
// Native execution is cancelled only after its client-visible state is durable.
func (m *Manager) archiveWaitingSession(ctx context.Context, s *session) error {
	if m.disk == nil || s.proc == nil || s.internal {
		return fail(503, "session_cache_unavailable", "AGY 等待中的工具交互暂时无法转存")
	}
	s.mu.Lock()
	if s.loading || s.waitAt.IsZero() || len(s.history) == 0 {
		s.mu.Unlock()
		return fail(409, "session_busy", "AGY 尚未进入可转存的工具等待状态")
	}
	for _, id := range pendingIDs(s) {
		c := s.pending[id]
		if !c.Delivered || c.Returned || c.Replayed {
			continue
		}
		if c.Replying > 0 {
			s.mu.Unlock()
			return fail(409, "session_busy", "AGY 正在交付工具结果，请稍后转存")
		}
	}
	s.mu.Unlock()
	v := closingCheckpoint(s, nil)
	if len(v.WaitingTools) == 0 {
		// Native execution may have consumed the last result without ending
		// its turn. Preserve this orphaned generation as interrupted instead
		// of retaining an occupied worker forever or claiming a live waiter.
		v.Interrupted = true
	}
	err := m.disk.put(v, true)
	m.mu.Lock()
	m.cacheError = err != nil
	m.mu.Unlock()
	if err != nil {
		return fail(503, "session_cache_unavailable", "AGY 工具等待转存失败，原会话已保留")
	}
	s.mu.Lock()
	s.running = false
	s.persisted = true
	s.mu.Unlock()
	s.cancel()
	if s.proc.alive() {
		s.proc.notify("session/cancel", map[string]string{"sessionId": s.ID})
		// Releasing the MCP wait lets the official process close the native
		// session. Its persisted ID and pending client calls remain recoverable.
		_ = s.proc.call(ctx, "session/close", map[string]string{"sessionId": s.ID}, nil)
	}
	m.mu.Lock()
	if m.sessions[s.tenant] == s {
		delete(m.sessions, s.tenant)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) sweepSession(ctx context.Context, s *session, now time.Time) {
	if !s.requestMu.TryLock() {
		return
	}
	defer s.requestMu.Unlock()
	s.mu.Lock()
	waitExpired := !s.waitAt.IsZero() && now.Sub(s.waitAt) > m.cfg.ToolTimeout
	idle := !s.running && s.waitAt.IsZero() && !s.loading
	expired := !s.proc.alive() || waitExpired || idle && now.Sub(s.last) > 10*time.Minute
	s.mu.Unlock()
	if !expired {
		return
	}
	if waitExpired {
		// A failed save must keep the original waiter; retry on the next sweep.
		_ = m.archiveWaitingSession(ctx, s)
	} else if idle && s.proc.alive() {
		if m.archiveSession(ctx, s) == nil {
			m.mu.Lock()
			delete(m.sessions, s.tenant)
			m.mu.Unlock()
		}
	} else {
		_ = m.preserveSession(s, nil)
		m.drop(s)
	}
}
