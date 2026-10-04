package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestOAuthPresetChannelNamesAndLimits(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	for _, channel := range []string{"codex", "antigravity"} {
		for i := 0; i < MaxOAuthPresets; i++ {
			if _, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: fmt.Sprintf("%s-%d", channel, i), Channel: channel, Name: fmt.Sprintf("preset-%d", i), Payload: "{}"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: channel + "-overflow", Channel: channel, Name: "overflow", Payload: "{}"}); err == nil {
			t.Fatal("channel limit not enforced")
		}
		list, err := s.ListOAuthPresets(ctx, channel)
		if err != nil || len(list) != MaxOAuthPresets {
			t.Fatalf("list=%v err=%v", list, err)
		}
		for _, p := range list {
			if p.Channel != channel {
				t.Fatal("foreign preset listed")
			}
		}
	}
	updated, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "new", Channel: "antigravity", Name: "PRESET-0", Payload: "new"})
	if err != nil || updated.ID != "antigravity-0" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	codex, err := s.OAuthPreset(ctx, "codex-0")
	if err != nil || codex.Payload != "{}" {
		t.Fatalf("Codex overwritten: %+v err=%v", codex, err)
	}
}

func TestOAuthPresetLegacyChannelMigration(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	// Recreate the pre-channel schema and an existing preset.
	for _, statement := range []string{
		"DROP TABLE oauth_presets",
		`CREATE TABLE oauth_presets(id TEXT PRIMARY KEY,name TEXT NOT NULL COLLATE NOCASE UNIQUE,payload TEXT NOT NULL,updated_by TEXT NOT NULL DEFAULT '',created_at_ms INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL)`,
		`INSERT INTO oauth_presets VALUES('legacy','日常','{"version":1}','管理员',100,200)`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.migrate(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := s.OAuthPreset(ctx, "legacy")
	if err != nil || legacy.Channel != "codex" || legacy.Name != "日常" || legacy.Payload != `{"channel":"codex","version":2}` || legacy.CreatedAt.UnixMilli() != 100 || legacy.UpdatedAt.UnixMilli() != 200 {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
	if _, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "anti", Channel: "antigravity", Name: "日常", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListOAuthPresets(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "legacy" {
		t.Fatalf("legacy list=%+v err=%v", items, err)
	}
}

func TestOAuthPresetFormatUpgradePreservesConfigurationAndMetadata(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	payload := `{"version":1,"models":[{"id":"gpt-test","enabled":true}],"aliases":[{"name":"gpt-test","alias":"daily","fork":true}],"reasoning_caps":{"gpt-test":"high"},"extra":{"future":"kept"}}`
	for _, item := range []struct{ id, channel, payload string }{
		{"legacy", "codex", payload},
		{"scoped-v1", "antigravity", `{"version":1,"channel":"antigravity","models":[]}`},
		{"mismatch", "codex", `{"version":1,"channel":"antigravity","models":[]}`},
		{"bad", "codex", "{invalid"},
		{"future", "codex", `{"version":3,"channel":"codex","models":[]}`},
		{"invalid-v2", "codex", `{"version":2,"models":[]}`},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO oauth_presets(id,channel,name,payload,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?)`, item.id, item.channel, item.id, item.payload, "管理员", 100, 200); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx, true); err != nil {
		t.Fatal(err)
	}
	migrated, err := s.OAuthPreset(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	var oldFields, newFields map[string]any
	if err := json.Unmarshal([]byte(payload), &oldFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(migrated.Payload), &newFields); err != nil {
		t.Fatal(err)
	}
	oldFields["version"], oldFields["channel"] = float64(2), "codex"
	if !reflect.DeepEqual(oldFields, newFields) || migrated.UpdatedBy != "管理员" || migrated.CreatedAt.UnixMilli() != 100 || migrated.UpdatedAt.UnixMilli() != 200 {
		t.Fatalf("changed configuration or metadata: %+v", migrated)
	}
	scoped, err := s.OAuthPreset(ctx, "scoped-v1")
	if err != nil || scoped.Payload != `{"channel":"antigravity","models":[],"version":2}` {
		t.Fatalf("scoped=%+v err=%v", scoped, err)
	}
	before, err := s.ListOAuthPresets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx, true); err != nil {
		t.Fatal(err)
	}
	after, err := s.ListOAuthPresets(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("migration was not idempotent")
	}
	for id, want := range map[string]string{
		"mismatch":   `{"version":1,"channel":"antigravity","models":[]}`,
		"bad":        "{invalid",
		"future":     `{"version":3,"channel":"codex","models":[]}`,
		"invalid-v2": `{"version":2,"models":[]}`,
	} {
		got, err := s.OAuthPreset(ctx, id)
		if err != nil || got.Payload != want {
			t.Fatalf("unexpected repair of %s: %+v err=%v", id, got, err)
		}
	}
}
