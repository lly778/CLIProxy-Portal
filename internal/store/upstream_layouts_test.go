package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSharedUpstreamCardLayoutsPersistAndRemainSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layouts.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	for scope, order := range map[string][]string{
		"presets":             {"preset-b", "preset-a"},
		"aliases:codex":       {"model-c", "model-a"},
		"aliases:antigravity": {"gemini", "claude"},
	} {
		if err := s.SetUpstreamCardOrder(t.Context(), scope, order); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for scope, want := range map[string][]string{
		"presets":             {"preset-b", "preset-a"},
		"aliases:codex":       {"model-c", "model-a"},
		"aliases:antigravity": {"gemini", "claude"},
		"aliases:gemini":      {},
	} {
		got, err := s.UpstreamCardOrder(t.Context(), scope)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("scope=%s order=%v err=%v", scope, got, err)
		}
	}
	if err := s.SetUpstreamCardOrder(t.Context(), "presets", []string{"preset-a"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UpstreamCardOrder(t.Context(), "presets")
	if err != nil || !reflect.DeepEqual(got, []string{"preset-a"}) {
		t.Fatalf("latest shared write=%v err=%v", got, err)
	}
	if err := s.SetUpstreamCardOrder(t.Context(), "presets", nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.UpstreamCardOrder(t.Context(), "presets")
	if err != nil || len(got) != 0 {
		t.Fatalf("clear order=%v err=%v", got, err)
	}
}

func TestSharedLayoutRejectsInvalidScopesIDsAndDamagedStoredData(t *testing.T) {
	s := testStore(t)
	for _, scope := range []string{"", "registration_open", "aliases:", "aliases:CODEX", "aliases:../codex", "aliases:" + strings.Repeat("a", 65)} {
		if err := s.SetUpstreamCardOrder(t.Context(), scope, []string{"a"}); err == nil {
			t.Fatalf("accepted invalid scope %q", scope)
		}
	}
	for _, order := range [][]string{{""}, {" a"}, {"a", "a"}, {"a\nb"}, {strings.Repeat("a", 129)}, make([]string, 257)} {
		if err := s.SetUpstreamCardOrder(t.Context(), "presets", order); err == nil {
			t.Fatalf("accepted invalid order %v", order)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('upstream_card_layout:presets','{"bad":true}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpstreamCardOrder(t.Context(), "presets"); err == nil {
		t.Fatal("damaged layout silently accepted")
	}
}
