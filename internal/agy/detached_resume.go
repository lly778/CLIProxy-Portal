package agy

import (
	"context"
	"encoding/json"
	"time"
)

const recoveryOverlayLimit = 8
const recoveryOverlayBytes = 64 << 10
const recoveryUnknownOrigin = "bridge_unacknowledged_tail"
const recoveryUnknownLabel = "Bridge recovery notice: client result unavailable, execution UNKNOWN"
const recoveryUnknownInstruction = "Continue using the preserved results and explicit unknown-status recovery notices. Never automatically repeat an unacknowledged operation; verify its effects read-only first."

// These are fingerprints and positions in the authoritative server history,
// not a second copy of the conversation. The client never received these items.
type recoveryOverlay struct {
	At    int
	Items []string
}

func recoveryProjection(items []map[string]any, overlays []recoveryOverlay) ([]map[string]any, bool) {
	if len(overlays) == 0 {
		return items, true
	}
	if len(overlays) > recoveryOverlayLimit {
		return nil, false
	}
	out := make([]map[string]any, 0, len(items))
	next := 0
	for _, o := range overlays {
		if o.At < next || len(o.Items) == 0 || len(o.Items) > 24 || o.At+len(o.Items) > len(items) {
			return nil, false
		}
		for i, fp := range o.Items {
			if itemFingerprint(items[o.At+i]) != fp {
				return nil, false
			}
		}
		out = append(out, items[next:o.At]...)
		next = o.At + len(o.Items)
	}
	return append(out, items[next:]...), true
}

// A full-history caller may omit the lost server-only tail on every subsequent
// request. Restore it only after the complete projected prefix matches exactly.
func expandRecoveryHistory(s *session, items []map[string]any) []map[string]any {
	if len(s.recoveryOverlays) == 0 || prefix(s.history, requestFingerprints(items)) {
		return items
	}
	projected, ok := recoveryProjection(s.items, s.recoveryOverlays)
	if !ok || len(items) < len(projected) || !prefix(requestFingerprints(projected), requestFingerprints(items)) {
		return items
	}
	return append(append([]map[string]any(nil), s.items...), items[len(projected):]...)
}

// Only a terminal, unresolved client-tool batch can be omitted. Earlier
// messages, actual results, other execution sources and visible answers cannot.
// Caller holds s.mu, or owns an unshared cold candidate.
func detachedTailStart(s *session) (int, bool) {
	if s.internal || s.interrupted || len(s.items) < 3 || len(s.recoveryOverlays) >= recoveryOverlayLimit || len(s.officialActive) > 0 {
		return 0, false
	}
	missing := map[string]*pendingTool{}
	for id, c := range s.pending {
		if c.Returned || c.Replayed {
			continue
		}
		if c.remote != nil || !c.Delivered || c.Replying > 0 || c.OutputHash != "" || c.Output != nil || c.ResultItem != nil || c.Tool.Type != "function" && c.Tool.Type != "custom" {
			return 0, false
		}
		select {
		case <-c.Ready:
			return 0, false
		default:
		}
		missing[id] = c
	}
	if len(missing) == 0 || len(missing) > 8 {
		return 0, false
	}
	cut := len(s.items)
	seen := map[string]bool{}
	for cut > 0 {
		v := s.items[cut-1]
		kind := stringField(v, "type")
		if kind == "reasoning" {
			cut--
			continue
		}
		id := resultCallID(v)
		if (kind == "function_call" || kind == "custom_tool_call") && missing[id] != nil && !seen[id] {
			c := missing[id]
			if itemFingerprint(v) != itemFingerprint(pendingCheckpointItem(c)) {
				return 0, false
			}
			seen[id] = true
			cut--
			continue
		}
		break
	}
	if len(seen) != len(missing) || cut < 2 || len(s.items)-cut+len(missing) > 24 {
		return 0, false
	}
	if data, _ := json.Marshal(s.items[cut:]); len(data) > recoveryOverlayBytes {
		return 0, false
	}
	h, err := inspectToolHistory(s.items[:cut])
	if err != nil || len(h.Open) > 0 || len(h.Records) == 0 {
		return 0, false
	}
	return cut, true
}

func detachedTailMatch(s *session, r request) (int, bool) {
	if r.Previous != "" || isCodexCompactionRequest(r) || s.model != r.Model {
		return 0, false
	}
	system, err := systemText(r)
	if err != nil || s.system != digest(system) {
		return 0, false
	}
	cut, ok := detachedTailStart(s)
	if !ok {
		return 0, false
	}
	base, ok := recoveryProjection(s.items[:cut], s.recoveryOverlays)
	if !ok || len(r.Items) <= len(base) || !prefix(requestFingerprints(base), requestFingerprints(r.Items)) || !onlyFollowupMessages(r.Items[len(base):]) {
		return 0, false
	}
	return cut, true
}

func savedDetachedKey(d *sessionDiskCache, v savedSession) string {
	s := &session{items: v.Items, recoveryOverlays: v.RecoveryOverlays, interrupted: v.Interrupted, pending: map[string]*pendingTool{}}
	for _, w := range v.WaitingTools {
		if w.Ready || w.Remote != nil || w.Output != nil {
			return ""
		}
		s.pending[w.ID] = &pendingTool{ID: w.ID, Tool: w.Tool, Arguments: w.Arguments, Delivered: true, Ready: make(chan struct{})}
	}
	cut, ok := detachedTailStart(s)
	if !ok {
		return ""
	}
	base, ok := recoveryProjection(v.Items[:cut], v.RecoveryOverlays)
	if !ok {
		return ""
	}
	keys := d.historyKeys(routingFingerprints(base))
	if len(keys) == 0 {
		return ""
	}
	return keys[len(keys)-1]
}

func isRecoveryUnknown(v any) bool {
	if text, ok := v.(string); ok {
		var decoded any
		if unmarshalExact([]byte(text), &decoded) != nil {
			return false
		}
		v = decoded
	}
	node, ok := v.(map[string]any)
	return ok && node["portal_result_origin"] == recoveryUnknownOrigin && node["execution_state"] == "unknown"
}

func recoveryUnknownResult() any {
	return map[string]any{"portal_result_origin": recoveryUnknownOrigin, "execution_state": "unknown", "isError": true, "content": []any{map[string]string{"type": "text", "text": "Bridge recovery notice, not a client tool result: the previous connection ended and no result was acknowledged for this operation. Execution and outcome are UNKNOWN, not successful or confirmed failed. Do not automatically repeat this action. Verify its effects read-only before further work."}}}
}

// Complete validations before touching a waiter. A live waiter is first saved
// and closed at the existing archive boundary, then loads the SAME native ID.
func (m *Manager) coldDetachedSession(ctx context.Context, s *session, r *request) (*session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.proc == nil {
		if err := m.prepareDetachedResumeMode(s, r, true); err != nil {
			return nil, err
		}
		return s, nil
	}
	if m.disk == nil || !s.requestMu.TryLock() {
		return nil, fail(409, "session_busy", "原会话仍在处理请求，请稍后续接")
	}
	defer s.requestMu.Unlock()
	if err := m.prepareDetachedResumeMode(s, r, true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	origins := map[string]bool{}
	for _, c := range s.pending {
		if c.traceRequest != "" {
			origins[c.traceRequest] = true
		}
	}
	s.mu.Unlock()
	m.replayMu.Lock()
	activeDelivery := false
	for _, e := range m.replays {
		e.mu.Lock()
		if origins[e.originRequest] && e.readers > 0 {
			activeDelivery = true
		}
		e.mu.Unlock()
	}
	m.replayMu.Unlock()
	if activeDelivery {
		return nil, fail(409, "session_busy", "原回复仍有客户端接收，不能撤销工具等待")
	}
	if err := m.archiveWaitingSessionMode(ctx, s, true); err != nil {
		return nil, err
	}
	cold, err := m.disk.loadSession(s.ID, s.owner, time.Now().Add(-sessionRetention).UnixMilli())
	if err != nil || cold == nil {
		return nil, fail(503, "session_cache_unavailable", "中断会话已保留，恢复索引暂不可用")
	}
	return cold, nil
}

func (m *Manager) prepareDetachedResume(s *session, r *request) error {
	return m.prepareDetachedResumeMode(s, r, false)
}

func (m *Manager) prepareDetachedResumeMode(s *session, r *request, validateOnly bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut, ok := detachedTailMatch(s, *r)
	if !ok {
		return fail(409, "followup_history_changed", "中断会话状态已改变，原记录已保留")
	}
	if len(s.followups) > 0 {
		return fail(409, "session_busy", "原会话已有排队消息，未撤销工具等待")
	}
	base, _ := recoveryProjection(s.items[:cut], s.recoveryOverlays)
	followup := r.Items[len(base):]
	extra, err := promptBlocks(followup)
	if err != nil {
		return err
	}
	tools, loaded := requestedTools(s, *r)
	policy, err := requestToolPolicy(*r, tools)
	if err != nil {
		return err
	}
	update, hash, err := sessionToolDelta(s, tools)
	if err != nil {
		return err
	}
	if text := policy.text(); text != "" {
		extra = append([]any{map[string]string{"type": "text", "text": text}}, extra...)
	}
	var added []map[string]any
	var calls []*pendingTool
	for _, id := range pendingIDs(s) {
		c := s.pending[id]
		if c.Returned || c.Replayed {
			continue
		}
		calls = append(calls, c)
		item := map[string]any{"type": c.Tool.Type + "_call_output", "call_id": c.ID, "output": recoveryUnknownResult()}
		if c.Tool.Type == "function" {
			item["type"] = "function_call_output"
		} else {
			item["type"] = "custom_tool_call_output"
		}
		added = append(added, item)
	}
	overlayItems := append(append([]map[string]any(nil), s.items[cut:]...), added...)
	if data, _ := json.Marshal(overlayItems); len(data) > recoveryOverlayBytes {
		return fail(409, "followup_history_changed", "未确认输出过大，原调用保持等待")
	}
	// Validate the whole new native input before releasing old tool waits.
	var blocks []any
	for _, c := range calls {
		blocks = append(blocks, map[string]string{"type": "text", "text": "[" + recoveryUnknownLabel + "; context only, do not execute again] " + c.ID + " " + c.Tool.key()})
		b, _ := toolResultBlocks(recoveryUnknownResult())
		blocks = append(blocks, b...)
	}
	blocks = append(blocks, extra...)
	size := len(update) + len(recoveryUnknownInstruction)
	for _, b := range blocks {
		if v, ok := b.(map[string]string); ok && v["type"] == "text" {
			size += len(v["text"])
		}
	}
	if size > MaxText {
		return fail(413, "tool_update_too_large", "恢复提示与新增输入超过单次安全上限")
	}
	if validateOnly {
		return nil
	}
	for i, c := range calls {
		c.Output = added[i]["output"]
		c.OutputHash = digest(c.Output)
		c.ResultItem = added[i]
		c.Extra, c.Update = nil, ""
		if i == len(calls)-1 {
			c.Extra = extra
			c.Update = update
		}
		c.receipt("result_unknown")
		close(c.Ready)
	}
	s.recoveryOverlays = append(s.recoveryOverlays, recoveryOverlay{At: cut, Items: requestFingerprints(overlayItems)})
	s.items = append(append([]map[string]any(nil), s.items...), added...)
	s.history = requestFingerprints(s.items)
	r.Items = append(append([]map[string]any(nil), s.items...), followup...)
	commitToolCatalog(s, tools, loaded, hash)
	s.policy = policy
	commitRemoteConnections(s, r)
	if r.ToolsProvided {
		s.deferredTools = deferredCatalog(r.Tools)
	}
	return nil
}

// Protect only explicitly uncertain operations, never ordinary same-argument
// calls. Read-only verification may proceed through another client tool.
func uncertainRecoveryAction(s *session, t tool, args map[string]any) bool {
	for _, o := range s.recoveryOverlays {
		end := o.At + len(o.Items)
		if end > len(s.items) {
			continue
		}
		calls := map[string]map[string]any{}
		for _, v := range s.items[o.At:end] {
			kind := stringField(v, "type")
			id := resultCallID(v)
			if kind == "function_call" || kind == "custom_tool_call" {
				calls[id] = v
				continue
			}
			if !isRecoveryUnknown(v["output"]) {
				continue
			}
			call := calls[id]
			if call == nil {
				continue
			}
			name := stringField(call, "name")
			if ns := stringField(call, "namespace"); ns != "" {
				name = ns + "." + name
			}
			if name != t.key() {
				continue
			}
			var original any
			if kind == "custom_tool_call_output" {
				original = map[string]any{"input": call["input"]}
			} else {
				raw, ok := call["arguments"].(string)
				if !ok || unmarshalExact([]byte(raw), &original) != nil {
					continue
				}
			}
			if digest(original) == digest(args) {
				return true
			}
		}
	}
	return false
}

func cloneRecoveryOverlays(v []recoveryOverlay) []recoveryOverlay {
	out := make([]recoveryOverlay, len(v))
	for i, o := range v {
		out[i] = recoveryOverlay{At: o.At, Items: append([]string(nil), o.Items...)}
	}
	return out
}

// Used when the native ID changes for a separately authorized history/model
// change. Keep server-only inserts only if every retained position still fits.
func carryRecoveryOverlays(next *session, items []map[string]any, old []recoveryOverlay) {
	if _, ok := recoveryProjection(items, old); ok {
		next.recoveryOverlays = cloneRecoveryOverlays(old)
	}
}
