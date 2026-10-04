package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

type recoveryCPA struct {
	cpamp.API
	files               []cpamp.AuthFile
	models              []cpamp.Model
	filesErr, modelsErr error
	reads               int
}

func (c *recoveryCPA) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	return c.files, c.filesErr
}

func (c *recoveryCPA) ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error) {
	c.reads++
	return c.models, c.modelsErr
}

func TestRestoreSchedulableModels(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 9, 0, 0, time.UTC)
	active := cpamp.AuthFile{Name: "account.json", Provider: "antigravity", AuthIndex: "opaque", Status: "active", AvailabilityKnown: true}
	original := []cpamp.Model{{ID: "existing", OwnedBy: "openai"}}
	for _, tt := range []struct {
		name                string
		modify              func(*cpamp.AuthFile)
		filesErr, modelsErr error
		wantRestore         bool
	}{
		{name: "expired projection", wantRestore: true},
		{name: "deadline equality", modify: func(f *cpamp.AuthFile) {
			f.Cooldowns = []cpamp.AuthCooldown{{Scope: "model", ModelKey: "gemini-model", RetryAt: now}}
		}, wantRestore: true},
		{name: "future model cooldown", modify: func(f *cpamp.AuthFile) {
			f.Cooldowns = []cpamp.AuthCooldown{{Scope: "model", ModelKey: "gemini-model", RetryAt: now.Add(time.Minute)}}
		}},
		{name: "credential cooldown", modify: func(f *cpamp.AuthFile) {
			f.Cooldowns = []cpamp.AuthCooldown{{Scope: "credential", RetryAt: now.Add(time.Minute)}}
		}},
		{name: "unknown retry", modify: func(f *cpamp.AuthFile) {
			f.Cooldowns = []cpamp.AuthCooldown{{Scope: "model", ModelKey: "gemini-model"}}
		}},
		{name: "disabled", modify: func(f *cpamp.AuthFile) { f.Disabled = true }},
		{name: "unavailable", modify: func(f *cpamp.AuthFile) { f.Unavailable = true }},
		{name: "auth failure", modify: func(f *cpamp.AuthFile) { f.Status = "error" }},
		{name: "missing snapshot", modify: func(f *cpamp.AuthFile) { f.AvailabilityKnown = false }},
		{name: "missing identity", modify: func(f *cpamp.AuthFile) { f.AuthIndex = "" }},
		{name: "other provider", modify: func(f *cpamp.AuthFile) { f.Provider = "codex" }},
		{name: "management failure", filesErr: errors.New("offline")},
		{name: "registered models failure", modelsErr: errors.New("offline")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := active
			if tt.modify != nil {
				tt.modify(&file)
			}
			client := &recoveryCPA{files: []cpamp.AuthFile{file, file}, models: []cpamp.Model{{ID: "existing"}, {ID: "gemini-model"}, {ID: "GEMINI-MODEL"}, {ID: "bad\nmodel"}}, filesErr: tt.filesErr, modelsErr: tt.modelsErr}
			keys := NewKeys(nil, client)
			keys.Now = func() time.Time { return now }
			got := keys.RestoreSchedulableModels(t.Context(), original)
			want := original
			if tt.wantRestore {
				want = append(append([]cpamp.Model(nil), original...), cpamp.Model{ID: "gemini-model", Object: "model", OwnedBy: "antigravity"})
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got, want)
			}
			if client.reads > 1 {
				t.Fatal("duplicate credential queried twice")
			}
			if len(original) != 1 || original[0].OwnedBy != "openai" {
				t.Fatal("caller catalog mutated")
			}
		})
	}
}
