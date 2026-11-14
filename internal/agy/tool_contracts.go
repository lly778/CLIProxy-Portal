package agy

import "encoding/json"

const schemaProjectionVersion = 1

type toolContractDescription struct{ Catalog, Definition, Text string }

// The current allowlist and policy are checked before this cache is consulted.
// Cache ownership is the session, and catalog changes invalidate each entry.
func describeContract(s *session, t tool) (any, bool) {
	if s.contractCache == nil {
		s.contractCache = map[string]toolContractDescription{}
	}
	catalog := s.toolHash
	if catalog == "" {
		catalog = digest(s.tools)
	}
	entry, hit := s.contractCache[t.key()]
	hit = hit && entry.Catalog == catalog && entry.Definition == digest(t)
	if !hit {
		data, err := json.Marshal(t)
		if err != nil || len(data) > MaxText {
			return mcpError("Original client tool contract exceeds the readable limit."), false
		}
		entry = toolContractDescription{Catalog: catalog, Definition: digest(t), Text: string(data)}
		if len(s.contractCache) >= 16 || s.contractCacheBytes+len(entry.Text) > 1<<20 {
			s.contractCache = map[string]toolContractDescription{}
			s.contractCacheBytes = 0
		}
		if old, ok := s.contractCache[t.key()]; ok {
			s.contractCacheBytes -= len(old.Text)
		}
		s.contractCache[t.key()] = entry
		s.contractCacheBytes += len(entry.Text)
	}
	execution := "client"
	if t.remoteBinding != nil {
		execution = "server"
	}
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": entry.Text}}, "_meta": map[string]any{"definition_hash": entry.Definition, "projection_version": schemaProjectionVersion, "execution": execution}}, hit
}
