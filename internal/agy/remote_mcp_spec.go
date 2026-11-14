package agy

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"
)

// Authorization is ephemeral and is never serialized with a declaration,
// contract, checkpoint, observation, or response.
type remoteMCPSpec struct {
	Label         string   `json:"server_label"`
	URL           string   `json:"server_url"`
	Allowed       []string `json:"allowed_tools"`
	Approval      string   `json:"require_approval"`
	authorization string
}

func parseRemoteMCPSpec(data []byte) (remoteMCPSpec, error) {
	var raw struct {
		Type          string          `json:"type"`
		Label         string          `json:"server_label"`
		URL           string          `json:"server_url"`
		Allowed       json.RawMessage `json:"allowed_tools"`
		Approval      json.RawMessage `json:"require_approval"`
		Authorization string          `json:"authorization"`
		Headers       map[string]any  `json:"headers"`
	}
	invalid := func() (remoteMCPSpec, error) {
		return remoteMCPSpec{}, fail(400, "invalid_remote_mcp", "远程 MCP 声明或授权范围无效")
	}
	if unmarshalExact(data, &raw) != nil || raw.Type != "mcp" || len(raw.Label) < 1 || len(raw.Label) > 64 || len(raw.URL) > 4096 || len(raw.Authorization) > 8192 || len(raw.Headers) > 0 {
		return invalid()
	}
	for _, r := range raw.Label {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-", r) {
			return invalid()
		}
	}
	u, err := url.Parse(raw.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" || strings.HasSuffix(u.Hostname(), ".") {
		return invalid()
	}
	if port := u.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return invalid()
		}
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !remotePublicIP(ip) {
		return invalid()
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return invalid()
	}
	for _, r := range raw.Authorization {
		if r < 0x21 || r > 0x7e {
			return invalid()
		}
	}
	spec := remoteMCPSpec{Label: raw.Label, URL: u.String(), Approval: "always", authorization: raw.Authorization}
	if len(raw.Approval) > 0 && string(raw.Approval) != "null" {
		if json.Unmarshal(raw.Approval, &spec.Approval) != nil || spec.Approval != "always" && spec.Approval != "never" {
			return invalid()
		}
	}
	if len(raw.Allowed) > 0 && string(raw.Allowed) != "null" {
		if json.Unmarshal(raw.Allowed, &spec.Allowed) != nil {
			return invalid()
		}
		if len(spec.Allowed) > 256 {
			return invalid()
		}
		seen := map[string]bool{}
		for _, name := range spec.Allowed {
			if name == "" || len(name) > 256 || seen[name] {
				return invalid()
			}
			seen[name] = true
		}
		if spec.Allowed == nil {
			spec.Allowed = []string{}
		}
	}
	return spec, nil
}

func (s remoteMCPSpec) permits(name string) bool {
	if s.Allowed == nil {
		return true
	}
	for _, allowed := range s.Allowed {
		if name == allowed {
			return true
		}
	}
	return false
}

// Public declaration identity excludes bearer tokens. Execution approval also
// binds credential identity separately, so replacing a credential cannot reuse
// an approval granted to another remote account.
func (s remoteMCPSpec) identity() string { return digest(s) }
func (s remoteMCPSpec) authorizationIdentity() string {
	return digest([]string{s.identity(), s.authorization})
}
