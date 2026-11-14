package agy

import (
	"encoding/json"
	"sort"
	"strings"
)

type serverSearchEvent struct{ Call, Output map[string]any }

func deferredCatalog(tools []tool) []tool {
	var out []tool
	for _, t := range tools {
		if t.Deferred && (t.Type == "function" || t.Type == "custom") && !excludedClientTool(t) {
			out = append(out, t)
		}
	}
	return out
}

func hasServerSearch(tools []tool) bool {
	for _, t := range tools {
		if t.Type == "tool_search" && t.Execution == "server" {
			return true
		}
	}
	return false
}

func authorizedServerTools(catalog, loaded []tool) []tool {
	versions := map[string]string{}
	for _, t := range catalog {
		t.Deferred = false
		versions[t.key()] = digest(t)
	}
	var out []tool
	for _, t := range loaded {
		t.Deferred = false
		if versions[t.key()] == digest(t) {
			out = append(out, t)
		}
	}
	return out
}

// This is lexical discovery of the caller's explicit catalog. It neither
// invokes a model nor contacts a remote service nor inspects server files.
func serverToolSearch(s *session, t tool, args map[string]any) (any, *serverSearchEvent) {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" || len(query) > 1024 {
		return mcpError("Search query is empty or too long."), nil
	}
	limit := int64(5)
	if raw, ok := args["max_results"]; ok {
		var valid bool
		limit, valid = numberInt(raw, 1, 16)
		if !valid {
			return mcpError("max_results must be between 1 and 16."), nil
		}
	}
	type candidate struct {
		Tool  tool
		Score int
	}
	var matches []candidate
	terms := strings.Fields(strings.ToLower(query))
	for _, entry := range s.deferredTools {
		score := 0
		text := strings.ToLower(entry.key() + " " + entry.Description)
		for _, term := range terms {
			if strings.Contains(text, term) {
				score++
			}
			if strings.Contains(strings.ToLower(entry.key()), term) {
				score += 2
			}
		}
		if score > 0 {
			matches = append(matches, candidate{entry, score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].Tool.key() < matches[j].Tool.key()
	})
	found := []tool{}
	descriptors := []any{}
	for _, c := range matches[:min(int(limit), len(matches))] {
		entry := c.Tool
		entry.Deferred = false
		found = append(found, entry)
		descriptors = append(descriptors, compactMCPDescriptor(map[string]any{"name": entry.key(), "description": entry.Description, "inputSchema": mcpInputSchema(entry.Parameters)}))
	}
	data, err := json.Marshal(map[string]any{"tools": descriptors, "execution": "server", "invoke_via": callToolName, "read_original_via": describeToolName})
	if err != nil || len(data) > MaxText/2 {
		return mcpError("Discovered contracts exceed the safe catalog size; narrow the query."), nil
	}
	loaded := mergeTools(found, s.loadedTools)
	next := mergeTools(s.tools, found)
	if len(next) > 256 {
		return mcpError("Active tool catalog exceeds its bound."), nil
	}
	commitToolCatalog(s, next, loaded, digest(next))
	s.policy.Used = true
	id := newID("search_")
	var wireTools []any
	encoded, _ := json.Marshal(found)
	_ = unmarshalExact(encoded, &wireTools)
	event := &serverSearchEvent{
		Call:   map[string]any{"id": newID("ts_"), "type": "tool_search_call", "status": "completed", "execution": "server", "call_id": id, "arguments": args},
		Output: map[string]any{"id": newID("tso_"), "type": "tool_search_output", "status": "completed", "execution": "server", "call_id": id, "tools": wireTools},
	}
	s.trace.emit(Observation{Kind: "bridge", State: "completed", Category: "server_tool_search", Tool: t.key(), CallID: id, ArgumentsHash: digest(args), DefinitionHash: digest(t), ProjectionVersion: schemaProjectionVersion})
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": string(data)}}}, event
}

func (o *response) search(event serverSearchEvent) error {
	for _, item := range []map[string]any{event.Call, event.Output} {
		index := len(o.output)
		o.output = append(o.output, item)
		if err := o.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
		if err := o.event("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
	}
	return nil
}
