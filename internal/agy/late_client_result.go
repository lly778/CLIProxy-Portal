package agy

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// An append-only correction, never a second wire result for the original call.
// Clients cannot submit this internal item type themselves.
const lateClientResultType = "portal_late_tool_result"

type recoveredClientCall struct {
	call, unknown, actual map[string]any
}

func validRecoveryOverlays(items []map[string]any, overlays []recoveryOverlay) bool {
	if len(overlays) > recoveryOverlayLimit {
		return false
	}
	next := 0
	for _, o := range overlays {
		if o.At < next || len(o.Items) == 0 || len(o.Items) > 24 || o.At+len(o.Items) > len(items) {
			return false
		}
		for i, fp := range o.Items {
			if itemFingerprint(items[o.At+i]) != fp {
				return false
			}
		}
		next = o.At + len(o.Items)
	}
	return true
}

// Only the exact client calls covered by our recovery overlays are eligible.
// A caller-provided UNKNOWN object cannot create a receipt correction route.
// Caller owns s.mu, or the items belong to an unshared disk candidate.
func recoveredClientCalls(items []map[string]any, overlays []recoveryOverlay) (map[string]recoveredClientCall, bool) {
	if !validRecoveryOverlays(items, overlays) {
		return nil, false
	}
	calls := map[string]recoveredClientCall{}
	for _, o := range overlays {
		local := map[string]map[string]any{}
		for _, item := range items[o.At : o.At+len(o.Items)] {
			id, kind := resultCallID(item), stringField(item, "type")
			if kind == "function_call" || kind == "custom_tool_call" {
				if id == "" || len(id) > 256 || local[id] != nil || calls[id].call != nil || stringField(item, "execution") != "" && item["execution"] != "client" {
					return nil, false
				}
				local[id] = item
			} else if isRecoveryUnknown(item["output"]) {
				call := local[id]
				if call == nil || !clientResultFor(call, item) || calls[id].call != nil {
					return nil, false
				}
				calls[id] = recoveredClientCall{call: call, unknown: item}
			}
		}
	}
	for _, item := range items {
		if item["type"] != lateClientResultType {
			continue
		}
		id := resultCallID(item)
		c, exists := calls[id]
		actual, ok := lateClientResult(item)
		if !exists || c.actual != nil || !ok || item["call_fingerprint"] != itemFingerprint(c.call) || item["unknown_fingerprint"] != itemFingerprint(c.unknown) || !clientResultFor(c.call, actual) {
			return nil, false
		}
		c.actual = actual
		calls[id] = c
	}
	return calls, true
}

func clientResultFor(call, result map[string]any) bool {
	kind := stringField(call, "type")
	expected := "function_call_output"
	if kind == "custom_tool_call" {
		expected = "custom_tool_call_output"
	} else if kind != "function_call" {
		return false
	}
	_, present := result["output"]
	execution := result["execution"]
	return present && result["type"] == expected && resultCallID(result) == resultCallID(call) && (execution == nil || execution == "" || execution == "client")
}

func lateClientResult(item map[string]any) (map[string]any, bool) {
	result, ok := item["result"].(map[string]any)
	if !ok || item["type"] != lateClientResultType || resultCallID(result) != resultCallID(item) || isRecoveryUnknown(result["output"]) {
		return nil, false
	}
	if result["type"] != "function_call_output" && result["type"] != "custom_tool_call_output" {
		return nil, false
	}
	if _, present := result["output"]; !present {
		return nil, false
	}
	return result, true
}

// Accept either the projected transcript, or an exact transcript retaining the
// original recovered calls. Only bridge-generated UNKNOWN/reasoning inserts
// may be absent in the latter; every call and visible message must still match.
// Receipts replacing UNKNOWN are returned separately, never written in place.
func recoveryClientTail(s *session, items []map[string]any) (tail, embedded []map[string]any, ok bool) {
	projected, valid := recoveryProjection(s.items, s.recoveryOverlays)
	if valid && prefix(requestFingerprints(projected), requestFingerprints(items)) {
		return items[len(projected):], nil, true
	}
	hidden := map[int]bool{}
	for _, o := range s.recoveryOverlays {
		for i := range o.Items {
			hidden[o.At+i] = true
		}
	}
	pos := 0
	for i, old := range s.items {
		if old["type"] == lateClientResultType {
			continue // The actual receipt occurred at the original result position.
		}
		if hidden[i] && isRecoveryUnknown(old["output"]) {
			if pos < len(items) && isToolResultItem(stringField(items[pos], "type")) && resultCallID(items[pos]) == resultCallID(old) {
				if itemFingerprint(items[pos]) != itemFingerprint(old) {
					embedded = append(embedded, items[pos])
				}
				pos++
			}
			continue
		}
		if pos < len(items) && itemFingerprint(items[pos]) == itemFingerprint(old) {
			pos++
			continue
		}
		if hidden[i] && old["type"] == "reasoning" {
			continue
		}
		return nil, nil, false
	}
	return items[pos:], embedded, true
}

func carriesRecoveredResult(s *session, items []map[string]any) bool {
	if len(s.recoveryOverlays) == 0 {
		return false
	}
	calls, ok := recoveredClientCalls(s.items, s.recoveryOverlays)
	if !ok || len(calls) == 0 {
		return false
	}
	if prefix(s.history, requestFingerprints(items)) {
		items = items[len(s.history):]
	}
	if fullHistoryInput(items) {
		tail, embedded, valid := recoveryClientTail(s, items)
		if valid {
			for _, item := range embedded {
				c := calls[resultCallID(item)]
				if c.actual == nil || itemFingerprint(c.actual) != itemFingerprint(item) {
					return true
				}
			}
			if len(tail) > 0 {
				items = tail
			}
		}
	}
	for _, item := range items {
		if isToolResultItem(stringField(item, "type")) && calls[resultCallID(item)].call != nil {
			return true
		}
	}
	return false
}

// Plan without changing session state. Recompute from the original wire input
// under requestMu before committing, so a racing turn cannot rewrite history.
func prepareLateClientResults(s *session, r *request) error {
	if r.handoffRetry {
		return nil // Already normalized by the original request recovery path.
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	input := r.Items
	if r.lateResultInput != nil {
		input = r.lateResultInput
	} else if !carriesRecoveredResult(s, input) {
		return nil
	}
	system, err := systemText(*r)
	if err != nil {
		return err
	}
	if s.model != r.Model || s.system != digest(system) || r.owner != "" && s.owner != r.owner || isCodexCompactionRequest(*r) {
		return fail(409, "late_result_context_changed", "迟到结果必须续接原模型与指令，原记录已保留")
	}
	if s.running || hasWaitingResults(s) || len(s.officialActive) > 0 || len(s.followups) > 0 {
		return fail(409, "session_busy", "原会话仍在处理请求，请本轮结束后补交结果")
	}
	calls, ok := recoveredClientCalls(s.items, s.recoveryOverlays)
	if !ok {
		return fail(409, "late_result_history_changed", "原调用恢复记录无法核对，未补交结果")
	}
	tail := input
	if fullHistoryInput(input) {
		suffix, embedded, valid := recoveryClientTail(s, input)
		if !valid {
			return fail(409, "late_result_history_changed", "补交历史与原对话不一致，原记录已保留")
		}
		tail = append(append([]map[string]any(nil), embedded...), suffix...)
	}
	next := append([]map[string]any(nil), s.items...)
	var accepted []map[string]any
	seen, receipts := map[string]bool{}, 0
	for _, item := range tail {
		kind := stringField(item, "type")
		if !isToolResultItem(kind) {
			role := stringField(item, "role")
			if (kind != "" && kind != "message") || role != "user" && role != "developer" && role != "system" {
				return fail(400, "invalid_late_result", "补交结果只能附带新的用户或指令消息")
			}
			next = append(next, item)
			continue
		}
		id := resultCallID(item)
		c, exists := calls[id]
		if !exists || seen[id] || !clientResultFor(c.call, item) || isRecoveryUnknown(item["output"]) {
			return fail(409, "invalid_late_result", "补交结果的原调用、类型或执行来源无法核对")
		}
		if _, err := toolResultBlocks(item["output"]); err != nil {
			return err
		}
		seen[id], receipts = true, receipts+1
		if c.actual != nil {
			if itemFingerprint(c.actual) != itemFingerprint(item) {
				return fail(409, "result_changed", "已有真实结果不同，原记录未改写")
			}
			continue // Identical retransmission: no second model turn or tool run.
		}
		next = append(next, map[string]any{"type": lateClientResultType, "call_id": id, "call_fingerprint": itemFingerprint(c.call), "unknown_fingerprint": itemFingerprint(c.unknown), "result": item})
		accepted = append(accepted, item)
	}
	if receipts == 0 {
		// The receipt may already occur in an exact full projected history.
		// Only acknowledge that exact history, or its valid message suffix.
		if !fullHistoryInput(input) {
			return fail(409, "invalid_late_result", "没有可归属的迟到结果")
		}
	}
	if len(next) > 4096 {
		return fail(413, "context_too_large", "补交后消息数量超过 AGY 接入限制")
	}
	if _, err := promptBlocksLimit(next, MaxBody); err != nil {
		return err
	}
	if _, err := promptBlocks(next[len(s.items):]); err != nil {
		return err
	}
	if data, err := json.Marshal(next); err != nil || len(data) > MaxBody*2 {
		return fail(413, "context_too_large", "补交结果超过会话安全大小限制")
	}
	if r.lateResultInput == nil {
		r.lateResultInput = append([]map[string]any(nil), input...)
	}
	r.Items = next
	r.lateResultAck = len(next) == len(s.items)
	r.lateResults = accepted
	return nil
}

// These blocks describe acknowledged evidence only; no call enters the native
// execution queue. The earlier UNKNOWN notice stays in the audit history.
func lateClientResultBlocks(item map[string]any) ([]any, error) {
	actual, ok := lateClientResult(item)
	if !ok {
		return nil, fail(400, "invalid_late_result", "迟到工具结果记录无效")
	}
	blocks, err := toolResultBlocks(actual["output"])
	if err != nil {
		return nil, err
	}
	label := "[Late acknowledged client tool result; context only, do not execute again] " + resultCallID(item) + ". This actual receipt supersedes the earlier missing-result notice for this call. Derive success or failure only from its actual content; receipt alone does not establish success. Keep an inconclusive outcome UNKNOWN and skip it by default. Do not repeat a completed operation. Other unknown calls remain unknown."
	return append([]any{map[string]string{"type": "text", "text": label}}, blocks...), nil
}

// Maintain only hashed routing identities at the existing archive boundaries.
func (d *sessionDiskCache) putLateResultRoutes(tx *sql.Tx, v savedSession) error {
	if _, err := tx.Exec(`DELETE FROM session_late_result_routes WHERE session_id=?`, v.ID); err != nil {
		return err
	}
	calls, ok := recoveredClientCalls(v.Items, v.RecoveryOverlays)
	if !ok {
		return errors.New("invalid AGY recovered client result state")
	}
	for id := range calls {
		if _, err := tx.Exec(`INSERT INTO session_late_result_routes(owner,model,call_key,session_id) VALUES(?,?,?,?)`, d.hash(v.Owner), d.hash(v.Model), d.hash("late-client-call:"+id), v.ID); err != nil {
			return err
		}
	}
	return nil
}

func (d *sessionDiskCache) findLateResultSession(owner string, r request, live map[string]bool) (*session, error) {
	var found *session
	seen := map[string]bool{}
	var keys []string
	cutoff := time.Now().Add(-sessionRetention).UnixMilli()
	for _, item := range r.Items {
		if !isToolResultItem(stringField(item, "type")) {
			continue
		}
		callKey := d.hash("late-client-call:" + resultCallID(item))
		if seen[callKey] {
			continue
		}
		seen[callKey] = true
		keys = append(keys, callKey)
	}
	// Batch a long full transcript instead of querying once per old tool result.
	for len(keys) > 0 {
		n := min(256, len(keys))
		cursor := ""
		for {
			args := []any{d.hash(owner), d.hash(r.Model)}
			for _, key := range keys[:n] {
				args = append(args, key)
			}
			args = append(args, cutoff, cursor)
			marks := strings.TrimSuffix(strings.Repeat("?,", n), ",")
			rows, err := d.db.Query(`SELECT DISTINCT s.id FROM session_late_result_routes r JOIN sessions s ON s.id=r.session_id WHERE r.owner=? AND r.model=? AND r.call_key IN (`+marks+`) AND s.ready=1 AND s.last_ms>=? AND s.id>? ORDER BY s.id LIMIT 32`, args...)
			if err != nil {
				return nil, err
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
				return nil, err
			}
			for _, id := range ids {
				if live[id] || found != nil && found.ID == id {
					continue
				}
				s, err := d.loadSession(id, owner, cutoff)
				if err != nil {
					return nil, err
				}
				if s == nil || !sessionMatchesRequest(s, r) {
					continue
				}
				if found != nil {
					return nil, fail(409, "ambiguous_session", "补交结果对应多个原会话，未自动合并")
				}
				found = s
			}
			if len(ids) < 32 {
				break
			}
			cursor = ids[len(ids)-1]
		}
		keys = keys[n:]
	}
	return found, nil
}
