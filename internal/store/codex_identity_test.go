package store

import (
	"path/filepath"
	"testing"
)

func TestCodexIdentitySettingsPersistAndIsolateChannels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := s.EnabledCodexIdentityChannels(t.Context())
	if err != nil || len(enabled) != 0 {
		t.Fatalf("default settings = %v, %v", enabled, err)
	}
	for _, channel := range []string{"antigravity", "claude"} {
		if err := s.SetCodexIdentityCompatibility(t.Context(), channel, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, channel := range []string{"codex", "", "Codex", "../codex"} {
		if err := s.SetCodexIdentityCompatibility(t.Context(), channel, true); err == nil {
			t.Fatalf("accepted forbidden channel %q", channel)
		}
	}
	if err := s.SetCodexIdentityCompatibility(t.Context(), "antigravity", false); err != nil {
		t.Fatal(err)
	}
	// Even a manually inserted Codex preference cannot enable the transformation.
	_, err = s.db.Exec(`INSERT INTO settings(key,value) VALUES('codex_identity_compatibility:codex','true')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	enabled, err = s.EnabledCodexIdentityChannels(t.Context())
	if err != nil || len(enabled) != 1 || !enabled["claude"] {
		t.Fatalf("persisted settings = %v, %v", enabled, err)
	}
}
