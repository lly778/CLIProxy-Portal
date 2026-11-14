package agy

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
)

type CapabilityEvidence struct{ State, Source string }
type RuntimeEvidence struct {
	SHA256, Agent, Version string
	ACP                    int
	Capabilities           map[string]CapabilityEvidence
}

// Exact executable tested with an isolated ACP process and read-only account
// mount on 2026-11-14. No prompts were sent. Do not inherit these observations
// from version strings or apply them to a replacement executable.
const verifiedAGY153 = "8b4fc29f938cab08c47e7471238184fe77a6c9757e0d640bf980f133998a6f7c"

func (p *process) capabilitySupported(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evidence.Capabilities[name].State == "supported"
}

func runtimeFingerprint(command string) string {
	f, err := os.Open(command)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<30 {
		return ""
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func safeRuntimeLabel(s string) string {
	if len(s) > 80 {
		return ""
	}
	for _, c := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._+- /", c) {
			return ""
		}
	}
	return s
}

func newRuntimeEvidence(hash, agent, version string, protocol int, caps map[string]any) RuntimeEvidence {
	e := RuntimeEvidence{SHA256: hash, Agent: safeRuntimeLabel(agent), Version: safeRuntimeLabel(version), ACP: protocol, Capabilities: map[string]CapabilityEvidence{}}
	for _, key := range []string{"session_load", "mcp_http", "mcp_sse", "dynamic_tool_refresh", "mcp_over_acp", "native_turn_identity"} {
		e.Capabilities[key] = CapabilityEvidence{"unknown", "not_reported_or_probed"}
	}
	if load, ok := caps["loadSession"].(bool); ok {
		state := "unsupported"
		if load {
			state = "supported"
		}
		e.Capabilities["session_load"] = CapabilityEvidence{state, "acp_declaration"}
	}
	if mcp, ok := caps["mcpCapabilities"].(map[string]any); ok {
		for key, capability := range map[string]string{"http": "mcp_http", "sse": "mcp_sse", "acp": "mcp_over_acp"} {
			if yes, ok := mcp[key].(bool); ok {
				state := "unsupported"
				if yes {
					state = "supported"
				}
				e.Capabilities[capability] = CapabilityEvidence{state, "acp_declaration"}
			}
		}
	}
	if hash == verifiedAGY153 {
		e.Capabilities["dynamic_tool_refresh"] = CapabilityEvidence{"unknown", "isolated_setup_probe_no_refetch_within_20s_2026_11_14"}
		e.Capabilities["native_load_requires_record_check"] = CapabilityEvidence{"supported", "isolated_setup_probe_missing_id_returns_success_2026_11_14"}
	}
	return e
}

func (p *process) catalogRefreshCandidate() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evidence.SHA256 == verifiedAGY153
}

func (p *process) runtimeEvidence() RuntimeEvidence {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.evidence
	e.Capabilities = map[string]CapabilityEvidence{}
	for key, value := range p.evidence.Capabilities {
		e.Capabilities[key] = value
	}
	return e
}

func (p *process) observeCapability(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.evidence.Capabilities == nil {
		p.evidence.Capabilities = map[string]CapabilityEvidence{}
	}
	p.evidence.Capabilities[name] = CapabilityEvidence{"supported", "observed_protocol_success"}
}
