package agy

import (
	"maps"
	"sort"
	"strings"
)

// Two requests may select separate decoded copies of the same disk record.
// Claim its lineage before mutating either copy or issuing session/load/new.
func (m *Manager) claimSessionRecovery(s *session) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restoring[s.ID] {
		return nil, fail(409, "session_busy", "原 AGY 会话正在恢复，请等待当前续接完成")
	}
	for _, current := range m.sessions {
		if current == s {
			continue
		}
		current.mu.Lock()
		conflict := current.ID == s.ID && s.ID != ""
		if current.owner == s.owner {
			for response := range s.responses {
				if _, exists := current.responses[response]; exists {
					conflict = true
					break
				}
			}
		}
		current.mu.Unlock()
		if conflict {
			return nil, fail(409, "session_busy", "原 AGY 会话已经恢复，请按最新会话状态重试")
		}
	}
	if m.restoring == nil {
		m.restoring = map[string]bool{}
	}
	id := s.ID
	m.restoring[id] = true
	return func() { m.mu.Lock(); delete(m.restoring, id); m.mu.Unlock() }, nil
}

// A result-only continuation identifies its original call even without
// previous_response_id. Preserve that session's history before suffix slicing
// or deciding to rebuild; otherwise a valid result can become a new task.
func mergeResultHistory(s *session, req *request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 || fullHistoryInput(req.Items) || prefix(s.history, requestFingerprints(req.Items)) {
		return false
	}
	for _, item := range req.Items {
		if isToolResultItem(stringField(item, "type")) && s.pending[resultCallID(item)] != nil {
			historical := map[string]string{}
			for _, old := range s.items {
				if isToolResultItem(stringField(old, "type")) {
					historical[resultCallID(old)] = itemFingerprint(old)
				}
			}
			tail := req.Items
			seen := map[string]bool{}
			duplicate := false
			for _, next := range tail {
				if isToolResultItem(stringField(next, "type")) {
					id := resultCallID(next)
					duplicate = duplicate || seen[id]
					seen[id] = true
				}
			}
			if !duplicate {
				filtered := make([]map[string]any, 0, len(tail))
				for _, next := range tail {
					if !isToolResultItem(stringField(next, "type")) || historical[resultCallID(next)] != itemFingerprint(next) {
						filtered = append(filtered, next)
					}
				}
				tail = filtered
			}
			req.Items = append(append([]map[string]any(nil), s.items...), tail...)
			return true
		}
	}
	return false
}

// Check a context change against the original tool contracts before cancelling
// the native waiter. Validation operates on a detached copy and cannot wake a
// tool or commit a policy update to the old session.
func validateRebuildResults(s *session, req request) error {
	s.mu.Lock()
	scratch := &session{tools: s.tools, advertisedTools: s.advertisedTools, loadedTools: s.loadedTools, toolHash: s.toolHash, policy: s.policy, items: s.items, history: s.history, pending: map[string]*pendingTool{}}
	unresolved := false
	for id, old := range s.pending {
		if old.remote != nil && !old.Returned && !old.Replayed {
			select {
			case <-old.Ready:
				found := false
				for _, item := range req.Items {
					if old.ResultItem != nil && itemFingerprint(item) == itemFingerprint(old.ResultItem) {
						found = true
						break
					}
				}
				if !found {
					s.mu.Unlock()
					return fail(409, "remote_operation_pending", "原远程 MCP 结果尚未完整进入当前历史，请先续接原会话")
				}
			default:
				s.mu.Unlock()
				return fail(409, "remote_operation_pending", "原远程 MCP 审批或操作尚未完成，请先续接原会话")
			}
		}
		if !old.Delivered {
			continue
		}
		copy := *old
		if old.remote != nil {
			remote := *old.remote
			remote.approvalReady = make(chan struct{})
			select {
			case <-old.remote.approvalReady:
				close(remote.approvalReady)
			default:
			}
			copy.remote = &remote
		}
		copy.trace = nil
		copy.Ready = make(chan struct{})
		select {
		case <-old.Ready:
			close(copy.Ready)
		default:
		}
		scratch.pending[id] = &copy
		unresolved = unresolved || !old.Returned && !old.Replayed
	}
	s.mu.Unlock()
	if !unresolved {
		return nil
	}
	return deliverToolsForRequest(scratch, req.Items, &req)
}

// Keep the actual accepted wire result in an eviction checkpoint, including
// results already consumed through MCP just before the HTTP handler failed.
// Ready is checked by the caller; a missing result is never manufactured.
func checkpointResult(c *pendingTool) map[string]any {
	if c.ResultItem != nil {
		return c.ResultItem
	}
	item := map[string]any{"call_id": c.ID, "type": "function_call_output", "output": c.Output}
	switch c.Tool.Type {
	case "custom":
		item["type"] = "custom_tool_call_output"
	case "tool_search":
		value, _ := c.Output.(map[string]any)
		item = map[string]any{"call_id": c.ID, "type": "tool_search_output", "execution": "client", "tools": value["tools"]}
	case "shell", "local_shell", "apply_patch":
		value, _ := c.Output.(map[string]any)
		item = maps.Clone(value)
		if item == nil {
			item = map[string]any{}
		}
		item["call_id"], item["type"] = c.ID, c.Tool.Type+"_call_output"
	case "computer", "computer_use_preview":
		item["type"] = "computer_call_output"
	}
	return item
}

func recordToolBatch(s *session, out *response) {
	s.mu.Lock()
	ids := []string{}
	for id, call := range s.pending {
		if call.Delivered && call.BatchID == out.batchID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	out.trace.emit(Observation{ID: digest([]string{s.owner, s.ID, out.batchID, "manifest"}), Kind: "batch", State: "dispatched", SessionID: s.ID, BatchID: out.batchID, CallIDs: ids})
}

// Called with s.mu held after validating every supplied result.
func missingToolResults(s *session, results map[string]any) error {
	byBatch := map[string][]string{}
	var missing []string
	for id, call := range s.pending {
		select {
		case <-receivedReady(call):
			continue
		default:
		}
		if !call.Delivered {
			continue
		}
		if _, ok := results[id]; ok {
			continue
		}
		missing = append(missing, id)
		byBatch[call.BatchID] = append(byBatch[call.BatchID], id)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	for batch, ids := range byBatch {
		sort.Strings(ids)
		s.trace.emit(Observation{Kind: "batch", State: "missing_results", BatchID: batch, CallIDs: ids})
	}
	return fail(400, "missing_tool_result", "当前批次缺少工具结果，原调用保持等待；call_id="+strings.Join(missing[:min(8, len(missing))], ","))
}
