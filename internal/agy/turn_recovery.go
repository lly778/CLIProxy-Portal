package agy

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

func copyItems(items []map[string]any) []map[string]any {
	b, _ := json.Marshal(items)
	var out []map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// A running turn receives the results through MCP, so messages alongside those
// results must travel with the last response, rather than being marked as seen.
func resultSuffix(s *session, items []map[string]any) ([]any, error) {
	last := -1
	for i, v := range items {
		if c := s.pending[resultCallID(v)]; c != nil && c.Delivered && isToolResultItem(stringField(v, "type")) {
			last = i
		}
	}
	if last < 0 {
		return nil, nil
	}
	start := 0
	if prefix(s.history, requestFingerprints(items)) {
		start = len(s.history)
	} else {
		start = last + 1
		if !fullHistoryInput(items) {
			start = 0
		}
	}
	extras := []map[string]any{}
	for _, v := range items[start:] {
		if isToolResultItem(stringField(v, "type")) {
			continue
		}
		role := stringField(v, "role")
		if role != "user" && role != "developer" && role != "system" {
			return nil, fail(400, "invalid_tool_followup", "工具结果之后只能追加用户消息或指令")
		}
		extras = append(extras, v)
	}
	return promptBlocks(extras)
}

func stringField(v map[string]any, key string) string { s, _ := v[key].(string); return s }

func (m *Manager) startPrompt(s *session, blocks []any) {
	s.mu.Lock()
	s.running = true
	s.reported = map[string]reportedTool{}
	s.officialActive = map[string]bool{}
	s.last = time.Now()
	s.mu.Unlock()
	go func() {
		var result promptResult
		err := s.proc.call(s.life, "session/prompt", map[string]any{"sessionId": s.ID, "prompt": blocks}, &result)
		select {
		case s.events <- agentEvent{Result: &result, Err: err}:
		case <-s.life.Done():
		}
	}()
}

// Called after native completion. Results already delivered to the live MCP
// call are not replayed. Late results (including images) become a new ACP input.
func lateResultPrompt(s *session) ([]any, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blocks := []any{}
	waiting := false
	for _, id := range pendingIDs(s) {
		c := s.pending[id]
		if c.Returned || c.Replayed {
			continue
		}
		select {
		case <-c.Ready:
			content, err := toolResultBlocks(c.Output)
			if err != nil {
				return nil, false, err
			}
			blocks = append(blocks, map[string]string{"type": "text", "text": "[Completed client tool result; context only, do not execute again] " + c.ID + " " + c.Tool.key()})
			blocks = append(blocks, content...)
			if c.Update != "" {
				blocks = append(blocks, map[string]string{"type": "text", "text": c.Update})
			}
			blocks = append(blocks, c.Extra...)
		default:
			waiting = true
		}
	}
	if waiting {
		return nil, true, nil
	}
	if len(blocks) > 0 {
		for _, c := range s.pending {
			if c.Delivered && !c.Returned {
				c.Replayed = true
			}
		}
		blocks = append(blocks, map[string]string{"type": "text", "text": "Continue the task using these actual client tool results. Do not repeat completed actions."})
	}
	return blocks, false, nil
}

func hasWaitingResults(s *session) bool {
	for _, c := range s.pending {
		if c.Delivered && !c.Returned && !c.Replayed {
			return true
		}
	}
	return false
}

func waitMCPReplies(ctx context.Context, s *session) error {
	s.mu.Lock()
	waits := []chan struct{}{}
	for _, c := range s.pending {
		if c.Replying > 0 && !c.Returned {
			waits = append(waits, c.ReplyDone)
		}
	}
	s.mu.Unlock()
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.life.Done():
			return fail(503, "worker_lost", "AGY 会话已终止")
		}
	}
	return nil
}

func addStepUsage(a, b Usage) Usage {
	return Usage{Input: a.Input + b.Input, Output: a.Output + b.Output, Cache: a.Cache + b.Cache, Reasoning: a.Reasoning + b.Reasoning, Total: a.Total + b.Total, Known: a.Known || b.Known}
}

func pendingIDs(s *session) []string {
	ids := []string{}
	for id := range s.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) rebuildSession(ctx context.Context, s *session, r request) (*session, error) {
	// Validate the replay before cancelling an existing task. History is input
	// only; tool-call entries never enter the client execution queue.
	if _, err := promptBlocksLimit(r.Items, MaxBody); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if r.Previous != "" && len(s.history) > 0 && len(s.items) == 0 && !fullHistoryInput(r.Items) {
		s.mu.Unlock()
		return nil, fail(409, "history_required", "旧版缓存缺少重建所需的完整历史，请由客户端发送完整对话或新建对话")
	}
	tools, loaded := requestedTools(s, r)
	responses := s.responses
	keys := s.attempts
	s.mu.Unlock()
	r.Tools = mergeTools(tools, loaded)
	r.ToolsProvided = true
	if _, err := requestToolPolicy(r, r.Tools); err != nil {
		return nil, err
	}
	if err := m.saveSession(s, false); err != nil {
		return nil, err
	}
	m.drop(s)
	next, err := m.create(ctx, s.owner, r)
	if err != nil {
		return nil, err
	}
	next.responses = responses
	next.attempts = keys
	return next, nil
}
