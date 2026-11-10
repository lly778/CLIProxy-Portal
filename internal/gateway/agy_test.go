package gateway

import (
	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/store"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAGYGatewayAuthorizationAndNoFallback(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "portal.db"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	const raw = "sk-agy-test"
	hash := cpamp.HashAPIKey(raw)
	u := domain.User{ID: "agy-user", Phone: "13800138000", Name: "test", PasswordHash: "unused", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now}
	if e = st.CreateUser(ctx, u); e != nil {
		t.Fatal(e)
	}
	if e = st.CreateKey(ctx, domain.APIKey{ID: "agy-key", UserID: u.ID, Hash: hash, LastFour: "test", Status: "active", IssuedAt: now}); e != nil {
		t.Fatal(e)
	}
	if e = st.SaveAGYSettings(ctx, agy.Settings{Enabled: true, AdminOnly: true, Models: []agy.Model{{ID: "gemini", Levels: []string{"low", "high"}}}, Rules: []agy.ModelRule{{ID: "gemini", Enabled: true, Aliases: []string{"friendly"}, Cap: "low"}}}, 0); e != nil {
		t.Fatal(e)
	}
	calls := 0
	secret := strings.Repeat("a", 64)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Portal-Owner") != u.ID+":"+hash {
			t.Error("untrusted owner or service auth")
		}
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "agy/gemini" || body.Reasoning.Effort != "low" {
			t.Error("alias or cap not applied")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/internal/compact" {
			w.Write([]byte(`{"id":"resp_agy","object":"response.compaction","output":[]}`))
		} else {
			w.Write([]byte(`{"id":"resp_agy","status":"completed","output":[]}`))
		}
	}))
	defer worker.Close()
	client, _ := agy.NewClient(worker.URL, secret)
	vault, e := NewVault(t.TempDir(), []byte(secret[:32]))
	if e != nil {
		t.Fatal(e)
	}
	g := &Gateway{store: st, AGY: client, vault: vault}
	for _, tc := range []struct {
		path, key string
		status    int
	}{{"/v1/responses", "invalid", 401}, {"/v1/responses/compact", "invalid", 401}, {"/v1/responses/compact", raw, 200}, {"/v1/responses", raw, 200}} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"agy/friendly","input":"test","reasoning":{"effort":"high"}}`))
		r.Header.Set("Authorization", "Bearer "+tc.key)
		w := httptest.NewRecorder()
		if !g.routeAGY(w, r, nil, nil) || w.Code != tc.status {
			t.Fatalf("%s status %d: %s", tc.path, w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("unexpected worker calls: %d", calls)
	}
	if e = st.SetUserStatus(ctx, u.ID, domain.StatusPending, ""); e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"agy/friendly","input":"test"}`))
	r.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	g.routeAGY(w, r, nil, nil)
	if w.Code != 403 || calls != 2 {
		t.Fatal("full rollout bypassed ordinary portal approval")
	}
}
