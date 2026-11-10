package service

import (
	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
	"context"
	"path/filepath"
	"testing"
)

func TestAGYPresetUsesLocalAtomicSettings(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "portal.db"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	ctx := context.Background()
	s := agy.Settings{Enabled: true, AdminOnly: true, Models: []agy.Model{{ID: "gemini", Levels: []string{"low", "high"}}, {ID: "other"}}, Rules: []agy.ModelRule{{ID: "gemini", Enabled: true}, {ID: "other"}}}
	if e = st.SaveAGYSettings(ctx, s, 0); e != nil {
		t.Fatal(e)
	}
	k := NewKeys(st, nil) // A CPA call would panic: AGY configuration is entirely local.
	snapshot := OAuthChannelPreset{Channel: "agy", Models: []OAuthPresetModel{{ID: "agy/gemini", Enabled: true}, {ID: "agy/other"}}, Aliases: []cpamp.OAuthModelAlias{{Name: "agy/gemini", Alias: "friendly", Fork: false}}, ReasoningCaps: map[string]string{"agy/gemini": "high"}}
	if e = k.applyOAuthChannelPreset(ctx, snapshot); e != nil {
		t.Fatal(e)
	}
	actual, e := k.oauthChannelPresetSnapshot(ctx, "agy")
	if e != nil {
		t.Fatal(e)
	}
	canonicalizeOAuthPreset(&snapshot)
	if !oauthChannelPresetsEqual(snapshot, actual) {
		t.Fatalf("preset round trip mismatch: %+v", actual)
	}
	updated, _ := st.AGYSettings(ctx)
	if _, ok := updated.Resolve("agy/friendly"); !ok {
		t.Fatal("alias did not resolve")
	}
	if _, ok := updated.Resolve("agy/gemini"); ok {
		t.Fatal("hidden original remained visible")
	}
	snapshot.ReasoningCaps["agy/gemini"] = "ultra"
	if e = k.applyOAuthChannelPreset(ctx, snapshot); e == nil {
		t.Fatal("undeclared effort accepted")
	}
	unchanged, _ := st.AGYSettings(ctx)
	if unchanged.Revision != updated.Revision {
		t.Fatal("invalid preset partially applied")
	}
}
