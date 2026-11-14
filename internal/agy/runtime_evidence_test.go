package agy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeEvidenceSeparatesDeclaredObservedUnknownAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture-runtime")
	if err := os.WriteFile(path, []byte("version one"), 0600); err != nil {
		t.Fatal(err)
	}
	hash := runtimeFingerprint(path)
	if len(hash) != 64 || runtimeFingerprint(filepath.Dir(path)) != "" {
		t.Fatal("invalid binary fingerprint")
	}
	p := &process{evidence: newRuntimeEvidence(hash, "agy", "1.5.3", 1, map[string]any{"loadSession": true, "mcpCapabilities": map[string]any{"http": true, "sse": false}})}
	first := p.runtimeEvidence()
	if first.Capabilities["session_load"].Source != "acp_declaration" || first.Capabilities["mcp_sse"].State != "unsupported" {
		t.Fatal("declared capability lost", first)
	}
	for _, name := range []string{"dynamic_tool_refresh", "mcp_over_acp", "native_turn_identity"} {
		if first.Capabilities[name].State != "unknown" {
			t.Fatal("unverified feature advertised", name)
		}
	}
	p.observeCapability("session_load")
	first.Capabilities["mcp_over_acp"] = CapabilityEvidence{"supported", "forged"}
	if p.runtimeEvidence().Capabilities["mcp_over_acp"].State != "unknown" || p.runtimeEvidence().Capabilities["session_load"].Source != "observed_protocol_success" {
		t.Fatal("evidence mutation or observation failed")
	}
	if err := os.WriteFile(path, []byte("version two"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement := newRuntimeEvidence(runtimeFingerprint(path), "agy", "", 1, nil)
	if replacement.SHA256 == hash || replacement.Capabilities["session_load"].State != "unknown" || p.runtimeEvidence().SHA256 != hash {
		t.Fatal("replacement inherited old process evidence")
	}
	if safeRuntimeLabel("secret\nvalue") != "" || safeRuntimeLabel(strings.Repeat("x", 81)) != "" {
		t.Fatal("unbounded runtime label exposed")
	}
}

func TestRuntimeSetupProbeEvidenceIsScopedAndDoesNotClaimModelAdoption(t *testing.T) {
	known := newRuntimeEvidence(verifiedAGY153, "agy", "1.5.3", 1, nil)
	if known.Capabilities["native_load_requires_record_check"].State != "supported" || known.Capabilities["dynamic_tool_refresh"].State != "unknown" || known.Capabilities["native_turn_identity"].State != "unknown" {
		t.Fatal("setup evidence overstated", known)
	}
	replaced := newRuntimeEvidence(strings.Repeat("0", 64), "agy", "1.5.3", 1, nil)
	if replaced.Capabilities["native_load_requires_record_check"].State == "supported" {
		t.Fatal("version label inherited executable-specific evidence")
	}
}

func TestMCPOverACPRequiresExplicitBooleanDeclaration(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{{true, "supported"}, {false, "unsupported"}, {nil, "unknown"}, {"true", "unknown"}, {map[string]any{}, "unknown"}} {
		e := newRuntimeEvidence("fixture", "agy", "1.5.3", 1, map[string]any{"mcpCapabilities": map[string]any{"http": true, "acp": tc.value}})
		if e.Capabilities["mcp_over_acp"].State != tc.want || e.Capabilities["mcp_http"].State != "supported" {
			t.Fatal("ACP transport capability was inferred or conflated", tc.want, e)
		}
		if tc.want != "unknown" && e.Capabilities["mcp_over_acp"].Source != "acp_declaration" {
			t.Fatal("declaration mislabeled as execution evidence")
		}
	}
}
