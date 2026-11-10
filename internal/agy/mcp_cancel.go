package agy

import "encoding/json"

// IDs remain raw JSON: numeric 1 and string "1" identify different calls.
func registerMCP(s *session, id string) (<-chan struct{}, func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpWaits == nil {
		s.mcpWaits = map[string]chan struct{}{}
	}
	if _, exists := s.mcpWaits[id]; exists {
		return nil, nil, false
	}
	ch := make(chan struct{})
	s.mcpWaits[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.mcpWaits[id] == ch {
			delete(s.mcpWaits, id)
		}
	}, true
}
func (m *Manager) cancelMCP(s *session, raw json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(raw, &params) != nil || len(params.RequestID) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch := s.mcpWaits[string(params.RequestID)]; ch != nil {
		delete(s.mcpWaits, string(params.RequestID))
		close(ch)
	}
}
func toolResultFailed(v any) bool {
	if text, ok := v.(string); ok {
		var value map[string]any
		if json.Unmarshal([]byte(text), &value) != nil {
			return false
		}
		v = value
	}
	value, ok := v.(map[string]any)
	if !ok {
		return false
	}
	return value["status"] == "failed" || value["status"] == "cancelled" || value["isError"] == true || value["is_error"] == true
}
