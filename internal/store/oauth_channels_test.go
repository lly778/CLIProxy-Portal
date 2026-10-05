package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestOAuthPresetNamesAreGlobal(t *testing.T) {
	s := testStore(t)
	first, err := s.SaveOAuthPreset(t.Context(), OAuthPreset{ID: "codex", Name: "Daily", Payload: `{"version":3,"channels":[{"channel":"codex","models":[]}]}`})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.SaveOAuthPreset(t.Context(), OAuthPreset{ID: "anti", Name: "DAILY", Payload: `{"version":3,"channels":[{"channel":"antigravity","models":[]}]}`})
	if err != nil || updated.ID != first.ID {
		t.Fatalf("global overwrite=%+v err=%v", updated, err)
	}
	list, err := s.ListOAuthPresets(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
}

func TestOAuthPresetLegacyGlobalMigration(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	for _, statement := range []string{
		"DROP TABLE oauth_presets",
		`CREATE TABLE oauth_presets(id TEXT PRIMARY KEY,name TEXT NOT NULL COLLATE NOCASE UNIQUE,payload TEXT NOT NULL,updated_by TEXT NOT NULL DEFAULT '',created_at_ms INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL)`,
		`INSERT INTO oauth_presets VALUES('legacy','日常','{"version":1,"models":[],"extra":"kept"}','管理员',100,200)`,
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
	if err != nil || legacy.Name != "日常" || legacy.CreatedAt.UnixMilli() != 100 || legacy.UpdatedAt.UnixMilli() != 200 || legacy.UpdatedBy != "管理员" {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
	var fields struct {
		Version  int
		Channels []map[string]any
	}
	if err := json.Unmarshal([]byte(legacy.Payload), &fields); err != nil || fields.Version != 3 || len(fields.Channels) != 1 || fields.Channels[0]["channel"] != "codex" || fields.Channels[0]["extra"] != "kept" {
		t.Fatalf("payload=%s err=%v", legacy.Payload, err)
	}
}

func TestOAuthPresetChannelMigrationMergesNamesAndPreservesSettings(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, `DROP INDEX idx_oauth_presets_global_name`); err != nil {
		t.Fatal(err)
	}
	codex := `{"version":2,"channel":"codex","models":[{"id":"gpt-test","enabled":true}],"aliases":[{"name":"gpt-test","alias":"daily","fork":true}],"reasoning_caps":{"gpt-test":"high"},"extra":{"future":"kept"}}`
	anti := `{"version":2,"channel":"antigravity","models":[{"id":"claude-test","enabled":false}],"aliases":[],"reasoning_caps":{}}`
	for _, entry := range []struct {
		id, channel, name, payload string
		updated                    int
	}{
		{"codex", "codex", "Daily", codex, 200}, {"anti", "antigravity", "DAILY", anti, 300},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO oauth_presets(id,channel,name,payload,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?)`, entry.id, entry.channel, entry.name, entry.payload, "管理员", 100, entry.updated); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx, true); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListOAuthPresets(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "anti" || list[0].UpdatedAt.UnixMilli() != 300 {
		t.Fatalf("merged=%+v err=%v", list, err)
	}
	var migrated struct {
		Version  int
		Channels []map[string]any
	}
	if err := json.Unmarshal([]byte(list[0].Payload), &migrated); err != nil || migrated.Version != 3 || len(migrated.Channels) != 2 {
		t.Fatalf("payload=%s err=%v", list[0].Payload, err)
	}
	var old map[string]any
	if err := json.Unmarshal([]byte(codex), &old); err != nil {
		t.Fatal(err)
	}
	delete(old, "version")
	if !reflect.DeepEqual(old, migrated.Channels[1]) {
		t.Fatalf("Codex settings changed: %v", migrated.Channels[1])
	}
	if err := s.migrate(ctx, true); err != nil {
		t.Fatal(err)
	}
	after, err := s.ListOAuthPresets(ctx)
	if err != nil || !reflect.DeepEqual(list, after) {
		t.Fatal("migration was not idempotent")
	}
}

func TestOAuthPresetMigrationRejectsMalformedWithoutDataLoss(t *testing.T) {
	for _, payload := range []string{`{invalid`, `{"version":2,"models":[]}`, `{"version":2,"channel":"antigravity","models":[]}`, `{"version":99}`, `{"version":3,"channels":[]}`} {
		t.Run(payload, func(t *testing.T) {
			s := testStore(t)
			ctx := t.Context()
			if _, err := s.db.ExecContext(ctx, `INSERT INTO oauth_presets(id,channel,name,payload,created_at_ms,updated_at_ms) VALUES('bad','codex','bad',?,100,200)`, payload); err != nil {
				t.Fatal(err)
			}
			if err := s.migrateOAuthPresetPayloads(ctx); err == nil || !strings.Contains(err.Error(), "迁移已取消") {
				t.Fatalf("migration error=%v", err)
			}
			got, err := s.OAuthPreset(ctx, "bad")
			if err != nil || got.Payload != payload {
				t.Fatalf("changed invalid data: %+v err=%v", got, err)
			}
		})
	}
}
