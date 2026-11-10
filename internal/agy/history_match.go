package agy

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
