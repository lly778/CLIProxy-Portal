package agy

import (
	"encoding/json"
	"slices"
)

const handoffRecoveryInstruction = "Resume the existing task from its preserved tool records; do not start the original request over. Inspect each recorded outcome first. Skip actions with confirmed successful results. For a confirmed failure, decide whether a fresh retry is appropriate using the actual error and current user instructions. Missing acknowledgements mean execution UNKNOWN, not failure. " + recoveryUnknownPolicy + " Never claim an action succeeded without evidence."

func appendRecoveryFailure(ids []string, id string) []string {
	if id == "" || slices.Contains(ids, id) {
		return ids
	}
	ids = append(ids, id)
	if len(ids) > 32 {
		ids = ids[len(ids)-32:]
	}
	return ids
}

func (m *Manager) linkRecoveryFailure(s *session, out *response) {
	if s == nil || !out.continuableFailure {
		return
	}
	s.mu.Lock()
	if s.responses == nil {
		s.responses = map[string]bool{}
	}
	s.responses[out.id] = true
	s.recoveryFailure = out.failureCode
	s.failedRecoveryResponses = appendRecoveryFailure(s.failedRecoveryResponses, out.id)
	s.mu.Unlock()
	m.mu.Lock()
	resident := m.sessions[s.tenant] == s
	m.mu.Unlock()
	if !resident {
		// A load/option error may happen before the regular interrupted-turn
		// cleanup is installed. It is still an error boundary, not normal I/O.
		_ = m.preserveSession(s, out)
	}
}

// Pure request preparation. Existing archive/load boundaries do the only writes
// and preserve the SAME native ID. No client changes or synthetic user turn.
func (m *Manager) prepareHandoffRetry(owner string, r *request) (*session, error) {
	s, err := m.handoffRetrySession(owner, r.replayKey)
	if err != nil || s == nil {
		return s, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.handoffRecoveries, r.replayKey) {
		return nil, fail(409, "handoff_recovery_exhausted", "本请求已尝试恢复，原状态已保留；可发送新消息继续，未重复执行未知操作")
	}
	cut, ok := handoffRetryTail(s, *r)
	if !ok {
		return nil, fail(409, "tool_handoff_pending", "原交接暂不能安全自动恢复，记录已保留；可按最新历史继续或补交原结果")
	}
	base, ok := recoveryProjection(s.items[:cut], s.recoveryOverlays)
	if !ok {
		return nil, fail(409, "followup_history_changed", "原恢复记录无法安全对齐，未改动工具结果")
	}
	r.handoffRetry, r.detachedResume = true, true
	r.Previous = "" // The exact request proof replaces its already-consumed base ID.
	r.Items = append([]map[string]any(nil), base...)
	return s, nil
}

// Unlike heuristic tail recovery, an exact request hash proves which accepted
// input produced this output. It can therefore recover a first tool call and
// retain already-received results without treating them as missing/failed.
// Caller holds s.mu; this function never mutates a waiter or history.
func handoffRetryTail(s *session, r request) (int, bool) {
	failedFollowup := r.recoveryResponse != "" && slices.Contains(s.failedRecoveryResponses, r.recoveryResponse) && s.responses[r.recoveryResponse]
	ownedRequest := r.replayKey != "" && (failedFollowup || s.requestReplayKey == r.replayKey)
	if !ownedRequest || s.owner != r.owner || s.model != r.Model || s.internal || s.interrupted || len(s.recoveryOverlays) >= recoveryOverlayLimit || len(s.officialActive) > 0 || s.remoteInFlight {
		return 0, false
	}
	system, err := systemText(r)
	if err != nil || s.system != digest(system) {
		return 0, false
	}
	active := map[string]*pendingTool{}
	for id, c := range s.pending {
		if c.Returned || c.Replayed {
			continue
		}
		if c.requestKey != s.requestReplayKey || !c.Delivered || c.remote != nil || c.Replying > 0 || c.Tool.Type != "function" && c.Tool.Type != "custom" {
			return 0, false
		}
		select {
		case <-c.Ready:
			if c.OutputHash == "" || c.ResultItem == nil || isRecoveryUnknown(c.Output) {
				return 0, false
			}
		default:
			if c.Output != nil || c.OutputHash != "" || c.ResultItem != nil {
				return 0, false
			}
		}
		active[id] = c
	}
	if len(active) == 0 || len(active) > 8 {
		return 0, false
	}
	cut := s.requestInputItems
	if cut == 0 {
		// Legacy records lack an input boundary. Only an exact terminal
		// reasoning/call/result run is eligible, never a visible answer.
		cut = len(s.items)
		for cut > 0 {
			item := s.items[cut-1]
			kind := stringField(item, "type")
			if kind != "reasoning" && active[resultCallID(item)] == nil {
				break
			}
			cut--
		}
	}
	if cut < 1 || cut >= len(s.items) {
		return 0, false
	}
	seen, results := map[string]bool{}, map[string]bool{}
	for _, item := range s.items[cut:] {
		kind, id := stringField(item, "type"), resultCallID(item)
		c := active[id]
		switch {
		case kind == "reasoning":
		case s.requestInputItems > 0 && stringField(item, "role") == "assistant" && (kind == "message" || kind == ""):
		case (kind == "function_call" || kind == "custom_tool_call") && c != nil:
			if seen[id] || itemFingerprint(item) != itemFingerprint(pendingCheckpointItem(c)) {
				return 0, false
			}
			seen[id] = true
		case isToolResultItem(kind) && c != nil && c.ResultItem != nil:
			if results[id] || !seen[id] || itemFingerprint(item) != itemFingerprint(c.ResultItem) {
				return 0, false
			}
			results[id] = true
		default:
			return 0, false
		}
	}
	if len(seen) != len(active) || len(s.items)-cut+len(active) > 24 {
		return 0, false
	}
	if data, _ := json.Marshal(s.items[cut:]); len(data) > recoveryOverlayBytes {
		return 0, false
	}
	history, err := inspectToolHistory(s.items[:cut])
	return cut, err == nil && len(history.Open) == 0
}

// A failed recovery remains a continuation point. A new user instruction may
// close its missing verification result as unknown rather than waiting forever.
func prepareFailedRecoveryFollowup(s *session, r *request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Previous == "" || !slices.Contains(s.failedRecoveryResponses, r.Previous) {
		return nil
	}
	active := false
	for _, c := range s.pending {
		active = active || c.Delivered && !c.Returned && !c.Replayed
	}
	if !active {
		return nil
	}
	next := *r
	next.handoffRetry, next.recoveryResponse = true, r.Previous
	cut, ok := handoffRetryTail(s, next)
	if !ok {
		return fail(409, "followup_history_changed", "核实失败记录已保留，当前输入不能安全接管原等待")
	}
	base, ok := recoveryProjection(s.items[:cut], s.recoveryOverlays)
	if !ok {
		return fail(409, "followup_history_changed", "原恢复记录无法安全对齐")
	}
	followup := r.Items
	if !onlyFollowupMessages(followup) {
		if len(followup) <= len(base) || !prefix(requestFingerprints(base), requestFingerprints(followup)) || !onlyFollowupMessages(followup[len(base):]) {
			return fail(409, "followup_history_changed", "新指令与原失败会话不一致，未改写真实结果")
		}
		followup = followup[len(base):]
	}
	next.Previous = ""
	next.Items = append(append([]map[string]any(nil), base...), followup...)
	*r = next
	return nil
}
