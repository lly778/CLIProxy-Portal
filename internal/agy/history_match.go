package agy

import (
	"maps"
	"strconv"
)

// Call IDs may be renamed when a client replays a transcript. Use their
// first-occurrence order only for candidate lookup; sessionMatchesRequest must
// still verify the complete transcript before accepting any candidate.
func routingFingerprints(items []map[string]any) []string {
	items = canonicalHistoryItems(items)
	aliases := map[string]string{}
	alias := func(id string) string {
		if aliases[id] == "" {
			aliases[id] = "history_call_" + strconv.Itoa(len(aliases))
		}
		return aliases[id]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		v := item
		if id := stringField(item, "call_id"); id != "" {
			v = maps.Clone(item)
			v["call_id"] = alias(id)
		}
		out = append(out, itemFingerprint(v))
	}
	return out
}

// The caller owns s.mu (or an unshared disk candidate). Memory and disk use the
// same acceptance rules; an index key alone is never sufficient for recovery.
func sessionMatchesRequest(s *session, r request) bool {
	canonicalSessionHistory(s)
	if r.Previous != "" {
		return s.responses[r.Previous]
	}
	for _, item := range r.Items {
		if id := resultCallID(item); id != "" && s.pending[id] != nil {
			return true
		}
	}
	if s.model != r.Model || len(s.history) == 0 {
		return false
	}
	if _, match := alignToolIDs(s, r.Items); match {
		return true
	}
	return !s.running && prefix(s.history, requestFingerprints(r.Items))
}

// Codex may put a reasoning item after the tool calls from the same assistant
// response when replaying history. Only canonicalize contiguous reasoning/call
// runs: never cross a user message, tool result, or visible assistant message.
// All item contents and the relative order of calls/reasoning remain intact.
func canonicalHistoryItems(items []map[string]any) []map[string]any {
	var out []map[string]any
	for start := 0; start < len(items); {
		end, calls, displaced := start, false, false
		for end < len(items) {
			kind := stringField(items[end], "type")
			if kind == "reasoning" {
				displaced = displaced || calls
			} else if isCallItem(kind) {
				calls = true
			} else {
				break
			}
			end++
		}
		if displaced {
			if out == nil {
				out = append([]map[string]any(nil), items...)
			}
			next := start
			for _, reasoning := range []bool{true, false} {
				for _, item := range items[start:end] {
					if (item["type"] == "reasoning") == reasoning {
						out[next] = item
						next++
					}
				}
			}
		}
		start = max(start+1, end)
	}
	if out != nil {
		return out
	}
	return items
}

// Keep the actual item positions in sync with fingerprints, so suffix slicing
// and renamed tool-ID matching still refer to the same records.
func canonicalSessionHistory(s *session) {
	if len(s.items) > 0 {
		items := canonicalHistoryItems(s.items)
		if &items[0] != &s.items[0] || len(s.history) != len(items) {
			s.history = requestFingerprints(items)
		}
		s.items = items
	}
}

// A renamed ID is accepted only when the entire preceding transcript still
// matches, including every function name, argument and result. Results alone
// can never establish an alias or cross an owner boundary.
func alignToolIDs(s *session, items []map[string]any) ([]map[string]any, bool) {
	if len(s.items) == 0 || len(items) < len(s.items) {
		return items, false
	}
	if prefix(s.history, requestFingerprints(items)) {
		return items, true
	}
	mapping := map[string]string{}
	reverse := map[string]string{}
	for i, old := range s.items {
		current := items[i]
		if !isCallItem(stringField(old, "type")) {
			continue
		}
		if old["type"] != current["type"] {
			return items, false
		}
		oldID, currentID := resultCallID(old), resultCallID(current)
		if oldID == "" || currentID == "" {
			return items, false
		}
		if previous := mapping[currentID]; previous != "" && previous != oldID {
			return items, false
		}
		if previous := reverse[oldID]; previous != "" && previous != currentID {
			return items, false
		}
		mapping[currentID] = oldID
		reverse[oldID] = currentID
	}
	if len(mapping) == 0 {
		return items, false
	}
	next := copyItems(items)
	for _, v := range next {
		if id := stringField(v, "call_id"); mapping[id] != "" {
			v["call_id"] = mapping[id]
		}
		if v["type"] == "local_shell_call" || v["type"] == "local_shell_call_output" {
			if id := stringField(v, "id"); mapping[id] != "" {
				v["id"] = mapping[id]
			}
		}
	}
	if !prefix(s.history, requestFingerprints(next)) {
		return items, false
	}
	return next, true
}

func isCallItem(kind string) bool {
	return kind == "function_call" || kind == "custom_tool_call" || kind == "tool_search_call" || isNativeCallItem(kind)
}

func fullHistoryInput(items []map[string]any) bool {
	for _, v := range items {
		if isCallItem(stringField(v, "type")) || v["role"] == "assistant" {
			return true
		}
	}
	return false
}
