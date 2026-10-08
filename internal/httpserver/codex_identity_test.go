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
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func TestAdminIdentityOptionChannelIsolationCSRFAndPersistence(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	admin, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	fake := &upstreamChannelAPI{excluded: map[string][]string{}, aliases: map[string][]cpamp.OAuthModelAlias{}}
	s, err := New(config.Config{CookieName: "portal_session", TimeZone: time.UTC, GatewayListenAddr: ":18318"}, st, accounts, service.NewKeys(st, fake), secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(s.Handler())
	defer portal.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login := getBody(t, client, portal.URL+"/login", http.StatusOK)
	csrf := extract(t, login, `name="csrf_token" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/login", url.Values{"csrf_token": {csrf}, "phone": {admin.Phone}, "password": {"very-long-admin-password"}}, http.StatusSeeOther)
	page := getBody(t, client, portal.URL+"/admin/upstreams?channel=antigravity", http.StatusOK)
	if strings.Count(page, `data-codex-identity-toggle`) != 1 || !strings.Contains(page, `action="/admin/upstreams/codex-identity"`) || !strings.Contains(page, "based on GPT-5") {
		t.Fatal("non-Codex option missing or leaked into Codex panel")
	}
	if strings.Index(page, "data-codex-identity-toggle") > strings.LastIndex(page, `class="oauth-model-list"`) || !strings.Contains(page, `type="checkbox" role="switch"`) || strings.Contains(page, "codex-identity-setting") {
		t.Fatal("compatibility must be a switch in the OAuth model header, with no separate card")
	}
	csrf = extract(t, page, `name="csrf_token" value="([^"]+)"`)
	endpoint := portal.URL + "/admin/upstreams/codex-identity"
	postForm(t, client, endpoint, url.Values{"channel": {"antigravity"}, "enabled": {"true"}}, http.StatusForbidden)
	for _, values := range []url.Values{
		{"channel": {"codex"}, "enabled": {"true"}},
		{"channel": {"antigravity"}, "enabled": {"invalid"}},
		{"channel": {"antigravity"}, "enabled": {"true", "false"}},
		{"channel": {"unknown"}, "enabled": {"true"}},
	} {
		values.Set("csrf_token", csrf)
		postForm(t, client, endpoint, values, http.StatusBadRequest)
	}
	postForm(t, client, endpoint, url.Values{"channel": {"antigravity"}, "enabled": {"true"}, "csrf_token": {csrf}}, http.StatusSeeOther)
	enabled, err := st.EnabledCodexIdentityChannels(t.Context())
	if err != nil || !enabled["antigravity"] || enabled["codex"] {
		t.Fatalf("saved settings = %v, %v", enabled, err)
	}
	page = getBody(t, client, portal.URL+"/admin/upstreams?channel=antigravity", http.StatusOK)
	if !strings.Contains(page, `name="enabled" value="true" checked`) {
		t.Fatal("saved state not reflected in page")
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(url.Values{"channel": {"antigravity"}, "enabled": {"true"}, "csrf_token": {csrf}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Enabled bool }
	decodeErr := json.NewDecoder(response.Body).Decode(&result)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "application/json") || decodeErr != nil || !result.Enabled {
		t.Fatalf("toggle JSON response: status=%d, result=%+v, err=%v", response.StatusCode, result, decodeErr)
	}
	s.Cfg.GatewayListenAddr = ""
	postForm(t, client, endpoint, url.Values{"channel": {"antigravity"}, "enabled": {"false"}, "csrf_token": {csrf}}, http.StatusServiceUnavailable)
	s.Cfg.GatewayListenAddr = ":18318"
	postForm(t, client, endpoint, url.Values{"channel": {"antigravity"}, "csrf_token": {csrf}}, http.StatusSeeOther)
	enabled, err = st.EnabledCodexIdentityChannels(t.Context())
	if err != nil || enabled["antigravity"] {
		t.Fatalf("disabled settings = %v, %v", enabled, err)
	}
}
