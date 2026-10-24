package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
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

func TestCachedPageRecoveryAlwaysReadsLiveCredentialState(t *testing.T) {
	var mode, registeredReads atomic.Int32
	now := time.Now().UTC()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			if mode.Load() == 4 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			file := map[string]any{"name": "account.json", "provider": "antigravity", "auth_index": "opaque", "status": "active", "unavailable": mode.Load() == 3, "disabled": mode.Load() == 2, "cooldowns": []any{}}
			if mode.Load() == 1 {
				file["cooldowns"] = []cpamp.AuthCooldown{{Scope: "credential", RetryAt: now.Add(time.Hour)}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{file}})
		case "/v0/management/auth-files/models":
			registeredReads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []cpamp.Model{{ID: "recovered-model"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	client, err := cpamp.New(upstream.URL, "test-management-key")
	if err != nil {
		t.Fatal(err)
	}
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	if _, err := client.ListAuthFiles(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewKeys(nil, client)
	keys.Now = func() time.Time { return now }
	original := []cpamp.Model{{ID: "base-model"}}
	if models := keys.RestoreSchedulableModels(ctx, original); len(models) != 2 || registeredReads.Load() != 1 {
		t.Fatal("active credential was not recovered from live registrations", models)
	}
	for _, state := range []int32{1, 2, 3, 4} {
		mode.Store(state)
		models := keys.RestoreSchedulableModels(ctx, original)
		if !reflect.DeepEqual(models, original) || registeredReads.Load() != 1 {
			t.Fatal("cached account metadata bypassed live cooldown, disable, availability or failed-read checks", state, models)
		}
	}
	mode.Store(0)
	if models := keys.RestoreSchedulableModels(ctx, original); len(models) != 2 || registeredReads.Load() != 2 {
		t.Fatal("cleared cooldown failed to restore models immediately", models)
	}
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
