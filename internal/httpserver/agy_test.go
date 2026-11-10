package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func TestAGYAdminAccountActionsAndSharedRefresh(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	keys := service.NewKeys(st, &clientModelsAPI{})
	paths := []string{}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/account/login":
			if r.Header.Get("X-Portal-Owner") != "admin:admin" {
				t.Error("admin identity not bound to login")
			}
			_, _ = w.Write([]byte(`{"id":"login-test","url":"https://accounts.google.com/o/oauth2/auth?response_type=code"}`))
		case "/internal/catalog":
			_ = json.NewEncoder(w).Encode([]agy.Model{{ID: "gemini", Levels: []string{"low", "high"}}})
		case "/internal/quota":
			_ = json.NewEncoder(w).Encode(agy.QuotaSnapshot{Account: agy.Account{LoggedIn: true, Email: "account@example.com"}})
		default:
			t.Error("unexpected worker request")
			w.WriteHeader(404)
		}
	}))
	defer worker.Close()
	keys.AGY, _ = agy.NewClient(worker.URL, strings.Repeat("s", 64))
	s, err := New(config.Config{CookieName: "portal_session"}, st, accounts, keys, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessions := map[string]string{}
	csrf := map[string]string{}
	for _, role := range []domain.Role{domain.RoleAdmin, domain.RoleUser} {
		id := "user"
		phone := "13800138001"
		if role == domain.RoleAdmin {
			id = "admin"
			phone = "13800138000"
		}
		u := domain.User{ID: id, Phone: phone, Name: id, PasswordHash: "unused", Role: role, Status: domain.StatusApproved, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err = st.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		sessions[id], csrf[id], err = accounts.CreateSession(t.Context(), u, "", "test")
		if err != nil {
			t.Fatal(err)
		}
	}
	post := func(id, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"csrf_token": {token}, "channel": {"codex"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", "application/json")
		r.AddCookie(&http.Cookie{Name: "portal_session", Value: sessions[id]})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := post("user", "/admin/upstreams/agy/account/login", csrf["user"]); w.Code != 403 {
		t.Fatalf("non-admin login status %d", w.Code)
	}
	if w := post("admin", "/admin/upstreams/agy/account/login", "invalid"); w.Code != 403 {
		t.Fatal("login accepted invalid CSRF")
	}
	if len(paths) != 0 {
		t.Fatal("unauthorized request reached official login")
	}
	if w := post("admin", "/admin/upstreams/agy/account/login", csrf["admin"]); w.Code != 200 {
		t.Fatalf("admin login failed: %d %s", w.Code, w.Body)
	}
	if w := post("admin", "/admin/upstreams/refresh", csrf["admin"]); w.Code != 200 {
		t.Fatalf("shared refresh failed: %d %s", w.Code, w.Body)
	}
	if strings.Join(paths, ",") != "/internal/account/login,/internal/catalog,/internal/quota?fresh=1" {
		t.Fatalf("global refresh did not read AGY models and quota once: %v", paths)
	}
	v, err := st.AGYSettings(t.Context())
	if err != nil || !v.Enabled || v.AdminOnly || len(v.Models) != 1 {
		t.Fatal("shared refresh did not persist the generally available catalog")
	}
}

func TestAGYAppearsInPageSummaryLegacyPresetEditorAndQuotaCarousel(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	admin := domain.User{ID: "admin-agy-summary", Phone: "13800138009", Name: "Admin", PasswordHash: "unused", Role: domain.RoleAdmin, Status: domain.StatusApproved, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = st.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	token, csrf, err := accounts.CreateSession(ctx, admin, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &upstreamChannelAPI{excluded: map[string][]string{}, aliases: map[string][]cpamp.OAuthModelAlias{}}
	keys := service.NewKeys(st, fake)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/account":
			_ = json.NewEncoder(w).Encode(agy.Account{LoggedIn: true, Email: "private-google@example.com"})
		case "/internal/quota":
			_ = json.NewEncoder(w).Encode(agy.QuotaSnapshot{Account: agy.Account{LoggedIn: true, Email: "private-google@example.com"}, UpdatedAt: time.Now(), Groups: []agy.QuotaGroup{{Label: "Gemini", Windows: []agy.QuotaWindow{{ID: "gemini-weekly", Label: "7D", RemainingPercent: 82, ResetAt: time.Now().Add(time.Hour)}}}}})
		default:
			t.Errorf("unnecessary worker request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer worker.Close()
	keys.AGY, _ = agy.NewClient(worker.URL, strings.Repeat("s", 64))
	if err = st.SaveAGYSettings(ctx, agy.Settings{Models: []agy.Model{{ID: "gemini-3.8-flash", Levels: []string{"low", "high"}}, {ID: "other"}}, Rules: []agy.ModelRule{{ID: "gemini-3.8-flash", Enabled: true, Aliases: []string{"friendly"}, Cap: "high"}, {ID: "other"}}}, 0); err != nil {
		t.Fatal(err)
	}
	// A saved preset created before AGY existed contains only its old channel.
	previous := service.OAuthPresetSnapshot{Version: service.OAuthPresetVersion, Channels: []service.OAuthChannelPreset{{Channel: "codex", Models: []service.OAuthPresetModel{{ID: "codex-model", Enabled: true}}}}}
	payload, _ := json.Marshal(previous)
	if _, err = st.SaveOAuthPreset(ctx, store.OAuthPreset{ID: "legacy-agy", Name: "Before AGY", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	s, err := New(config.Config{CookieName: "portal_session", TimeZone: time.UTC}, st, accounts, keys, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "portal_session", Value: token})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w
	}
	page := get("/admin/upstreams").Body.String()
	start := strings.Index(page, "上游模型配置与调用预设")
	if start < 0 || !strings.Contains(page[start:], "agy/gemini-3.8-flash") || !strings.Contains(page[start:], "agy/friendly") || !strings.Contains(page[start:], "上限 high") {
		t.Fatal("AGY summary missing its active model, alias or cap")
	}
	if !strings.Contains(page, "data-agy-account-open>账号管理") {
		t.Fatal("account button not renamed")
	}
	data := get("/admin/upstreams/presets/legacy-agy/edit")
	var editor struct {
		Revision string
		Channels []presetEditorChannel
	}
	if err = json.Unmarshal(data.Body.Bytes(), &editor); err != nil {
		t.Fatal(err)
	}
	target := service.OAuthPresetSnapshot{Version: service.OAuthPresetVersion}
	found := false
	for _, channel := range editor.Channels {
		entry := service.OAuthChannelPreset{Channel: channel.Channel}
		for _, model := range channel.Models {
			if channel.Channel == "agy" {
				if model.Enabled {
					t.Fatal("new provider silently enabled in a legacy preset")
				}
				if model.ID == "agy/gemini-3.8-flash" {
					found = true
					model.Enabled = true
					if len(model.ReasoningLevels) != 2 {
						t.Fatal("AGY thinking levels missing")
					}
					entry.Aliases = []cpamp.OAuthModelAlias{{Name: model.ID, Alias: "agy/new-friendly", Fork: true}}
					entry.ReasoningCaps = map[string]string{model.ID: "high"}
				}
			}
			entry.Models = append(entry.Models, service.OAuthPresetModel{ID: model.ID, Enabled: model.Enabled})
		}
		target.Channels = append(target.Channels, entry)
	}
	if !found {
		t.Fatal("AGY models missing from legacy preset editor")
	}
	encoded, _ := json.Marshal(target)
	form := url.Values{"csrf_token": {csrf}, "name": {"Before AGY"}, "revision": {editor.Revision}, "action": {"save"}, "payload": {string(encoded)}}
	r := httptest.NewRequest("POST", "/admin/upstreams/presets/legacy-agy/edit", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "portal_session", Value: token})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("save AGY draft: %d %s", w.Code, w.Body)
	}
	saved, _ := st.OAuthPreset(ctx, "legacy-agy")
	if !strings.Contains(saved.Payload, "agy/new-friendly") {
		t.Fatal("AGY preset changes not persisted")
	}
	live, _ := st.AGYSettings(ctx)
	if live.Rules[0].Aliases[0] != "friendly" {
		t.Fatal("saving a draft applied AGY routing changes")
	}
	pools := s.quotaPoolsView(keys.UpstreamQuotas(ctx), csrf, "/dashboard")
	found = false
	for _, p := range pools.Providers {
		if p.Provider == "Antigravity CLI（ACP）" {
			found = true
			if !p.Show || len(p.Groups) != 1 || p.Groups[0].RemainingPercent != 82 {
				t.Fatal("AGY quota carousel data incorrect")
			}
		}
	}
	if !found {
		t.Fatal("AGY missing from shared quota carousel")
	}
	data = get("/admin/upstreams/config/edit")
	if !strings.Contains(data.Body.String(), "agy/gemini-3.8-flash") {
		t.Fatal("live editor cache was polluted by page settings snapshot")
	}
}
