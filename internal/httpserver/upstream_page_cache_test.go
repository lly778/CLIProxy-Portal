package httpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func TestOverviewAndModelPagesShareConfigurationButKeepKeyChecksLive(t *testing.T) {
	const rawKey = "portal-live-key-for-cache-test"
	var mu sync.Mutex
	reads := map[string]int{}
	revoked := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		reads[r.Method+" "+r.URL.Path]++
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_, _ = io.WriteString(w, `{"files":[{"name":"codex.json","provider":"codex","auth_index":"credential-1","status":"active","disabled":false,"unavailable":false,"cooldowns":[]}]}`)
		case "/v0/management/oauth-model-alias":
			_, _ = io.WriteString(w, `{"oauth-model-alias":{"codex":[{"name":"gpt-cache","alias":"hidden-alias"}]}}`)
		case "/v0/management/oauth-excluded-models":
			_, _ = io.WriteString(w, `{"oauth-excluded-models":{"codex":[]}}`)
		case "/v0/management/model-definitions/codex":
			_, _ = io.WriteString(w, `{"models":[{"id":"gpt-cache"}]}`)
		case "/v0/management/api-keys":
			keys := []string{rawKey}
			if revoked {
				keys = nil
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"api-keys": keys})
		case "/v1/models":
			if revoked || r.Header.Get("Authorization") != "Bearer "+rawKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-cache","owned_by":"openai"},{"id":"hidden-alias","owned_by":"openai"}]}`)
		case "/v0/management/monitoring/analytics":
			_, _ = io.WriteString(w, `{"summary":{"total_calls":0,"total_tokens":0},"events":{"items":[],"total_count":0},"api_key_stats":[],"timeline":[]}`)
		case "/v1/chat/completions":
			if revoked || r.Header.Get("Authorization") != "Bearer "+rawKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		case "/v0/management/api-call":
			http.Error(w, "quota provider unavailable", http.StatusServiceUnavailable)
		default:
			if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v0/management/api-key-aliases/") {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	admin, err := accounts.CreateAdmin(t.Context(), "13900139111", "缓存管理员", "long-cache-test-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(t.Context(), domain.APIKey{ID: "cache-test-key", UserID: admin.ID, Hash: cpamp.HashAPIKey(rawKey), LastFour: "test", Alias: "cache-test", Status: "active", IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	api, err := cpamp.New(upstream.URL, "management-key")
	if err != nil {
		t.Fatal(err)
	}
	keys := service.NewKeys(st, api)
	s, err := New(config.Config{CookieName: "portal_session", TimeZone: time.UTC, CPAAPIBaseURL: upstream.URL}, st, accounts, keys, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(s.Handler())
	defer portal.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login := getBody(t, client, portal.URL+"/login", http.StatusOK)
	csrf := extract(t, login, `name="csrf_token" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/login", url.Values{"csrf_token": {csrf}, "phone": {admin.Phone}, "password": {"long-cache-test-password"}}, http.StatusSeeOther)
	for cycle := 0; cycle < 2; cycle++ {
		for _, page := range []string{"/dashboard", "/models", "/admin"} {
			body := getBody(t, client, portal.URL+page, http.StatusOK)
			if !strings.Contains(body, "gpt-cache") || strings.Contains(body, "hidden-alias") {
				t.Fatalf("page %s lost its catalog or exposed a hidden alias", page)
			}
			if page == "/models" {
				csrf = extract(t, body, `name="csrf_token" value="([^"]+)"`)
			}
		}
	}
	mu.Lock()
	for _, path := range []string{"/v0/management/oauth-model-alias", "/v0/management/oauth-excluded-models", "/v0/management/model-definitions/codex"} {
		if reads["GET "+path] != 1 {
			t.Errorf("cross-page configuration was fetched %d times: %s", reads["GET "+path], path)
		}
	}
	if reads["GET /v0/management/api-keys"] != 4 || reads["GET /v1/models"] != 4 || reads["GET /v0/management/auth-files"] <= 1 {
		t.Error("page caching skipped live key, model-list or credential-state verification")
	}
	catalogReads := reads["GET /v1/models"]
	mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		response, err := client.PostForm(portal.URL+"/models/test", url.Values{"csrf_token": {csrf}, "model": {"gpt-cache"}})
		if err != nil {
			t.Fatal(err)
		}
		var result modelTestResponse
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil || !result.OK {
			t.Fatal("manual model test stopped making live requests", err, result)
		}
	}
	mu.Lock()
	if reads["POST /v1/chat/completions"] != 2 || reads["GET /v1/models"] != catalogReads {
		t.Error("manual tests reused a response or unnecessarily rebuilt the catalog")
	}
	revoked = true
	mu.Unlock()
	body := getBody(t, client, portal.URL+"/models", http.StatusOK)
	if !strings.Contains(body, "API Key 已被外部撤销") {
		t.Fatal("warm configuration cache concealed an externally revoked user key")
	}
}
