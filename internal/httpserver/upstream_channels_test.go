package httpserver

import (
	"context"
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

type upstreamChannelAPI struct {
	cpamp.API
	antiDisabled       bool
	configCalls        int
	cacheInvalidations int
	configData         []byte
	excluded           map[string][]string
	aliases            map[string][]cpamp.OAuthModelAlias
}

func (f *upstreamChannelAPI) InvalidateUpstreamReadCache() { f.cacheInvalidations++ }

func (f *upstreamChannelAPI) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	return []cpamp.AuthFile{{Name: "codex.json", Provider: "codex", AccountSnapshot: "codex@example.com"}, {Name: "anti.json", Provider: "antigravity", AccountSnapshot: "anti@example.com", Disabled: f.antiDisabled}}, nil
}
func (f *upstreamChannelAPI) SetAuthFileDisabled(_ context.Context, file cpamp.AuthFile, disabled bool) error {
	if file.Provider == "codex" && !disabled {
		return nil // An enabled account can have its model registration refreshed.
	}
	if file.Provider != "antigravity" {
		panic("foreign account write")
	}
	f.antiDisabled = disabled
	return nil
}
func (f *upstreamChannelAPI) ListAuthFileModels(_ context.Context, file cpamp.AuthFile) ([]cpamp.Model, error) {
	name := file.Provider + "-model"
	for _, excluded := range f.excluded[file.Provider] {
		if excluded == name {
			return nil, nil
		}
	}
	models := []cpamp.Model{}
	mapped, keep := false, false
	for _, alias := range f.aliases[file.Provider] {
		if alias.Name == name {
			models = append(models, cpamp.Model{ID: alias.Alias})
			mapped, keep = true, keep || alias.Fork
		}
	}
	if !mapped || keep {
		models = append(models, cpamp.Model{ID: name})
	}
	return models, nil
}
func (f *upstreamChannelAPI) ListOAuthModelDefinitions(_ context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	return []cpamp.OAuthModelDefinition{{ID: channel + "-model", ThinkingLevels: []string{"low", "medium", "high"}}}, nil
}
func (f *upstreamChannelAPI) FetchAntigravityQuota(_ context.Context, _ cpamp.AuthFile, now time.Time) ([]cpamp.AntigravityQuotaWindow, error) {
	return []cpamp.AntigravityQuotaWindow{
		{ID: "claude-five", GroupID: "claude", Label: "Claude and GPT models · Five Hour Limit Remaining", RemainingPercent: 80, ObservedAt: now, ResetAt: now.Add(time.Hour)},
		{ID: "claude-week", GroupID: "claude", Label: "Claude and GPT models · Weekly Limit Remaining", RemainingPercent: 60, ObservedAt: now, ResetAt: now.Add(7 * 24 * time.Hour)},
		{ID: "gemini-five", GroupID: "gemini", Label: "Gemini Models · Five Hour Limit Remaining", RemainingPercent: 75, ObservedAt: now, ResetAt: now.Add(time.Hour)},
		{ID: "gemini-week", GroupID: "gemini", Label: "Gemini Models · Weekly Limit Remaining", RemainingPercent: 55, ObservedAt: now, ResetAt: now.Add(7 * 24 * time.Hour)},
	}, nil
}
func (f *upstreamChannelAPI) ListOAuthExcludedModels(_ context.Context, channel string) ([]string, error) {
	return f.excluded[channel], nil
}
func (f *upstreamChannelAPI) SetOAuthExcludedModels(_ context.Context, channel string, values []string) error {
	f.excluded[channel] = values
	return nil
}
func (f *upstreamChannelAPI) ListOAuthModelAliases(_ context.Context, channel string) ([]cpamp.OAuthModelAlias, error) {
	return f.aliases[channel], nil
}
func (f *upstreamChannelAPI) SetOAuthModelAliases(_ context.Context, channel string, values []cpamp.OAuthModelAlias) error {
	f.aliases[channel] = values
	return nil
}
func (f *upstreamChannelAPI) GetConfigYAML(context.Context) ([]byte, error) {
	f.configCalls++
	if len(f.configData) > 0 {
		return append([]byte(nil), f.configData...), nil
	}
	return []byte("api-keys: []\n"), nil
}
func (f *upstreamChannelAPI) PutConfigYAML(_ context.Context, data []byte) error {
	f.configData = append([]byte(nil), data...)
	return nil
}

func TestAdminUpstreamChannelFormsAndIsolation(t *testing.T) {
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
	keys := service.NewKeys(st, fake)
	s, err := New(config.Config{CookieName: "portal_session", TimeZone: time.UTC}, st, accounts, keys, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	page := getBody(t, client, portal.URL+"/admin/upstreams", http.StatusOK)
	for _, value := range []string{"Codex 凭证", "Antigravity 凭证", "codex@example.com", "anti@example.com", "配置模型与预设", "Gemini", "75%"} {
		if !strings.Contains(page, value) {
			t.Fatalf("stacked upstream page missing %s", value)
		}
	}
	if strings.Contains(page, "切换渠道") || strings.Contains(page, "刷新额度") {
		t.Fatal("obsolete channel/refresh controls remain")
	}
	csrf = extract(t, page, `name="csrf_token" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/admin/upstreams/refresh", url.Values{"channel": {"antigravity"}}, http.StatusForbidden)
	if fake.cacheInvalidations != 0 {
		t.Fatal("cache invalidated without CSRF")
	}
	postForm(t, client, portal.URL+"/admin/upstreams/refresh", url.Values{"csrf_token": {csrf}}, http.StatusSeeOther)
	if fake.cacheInvalidations != 1 {
		t.Fatal("refresh did not invalidate shared cache")
	}
	snapshot, err := keys.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	revision := service.OAuthConfigurationRevision(snapshot)
	for index := range snapshot.Channels {
		entry := &snapshot.Channels[index]
		if entry.Channel == "antigravity" {
			entry.Aliases = []cpamp.OAuthModelAlias{{Name: "antigravity-model", Alias: "anti-public", Fork: true}}
			entry.ReasoningCaps = map[string]string{"antigravity-model": "low"}
		}
	}
	payload, _ := json.Marshal(snapshot)
	postFormJSON(t, client, portal.URL+"/admin/upstreams/config/edit", url.Values{"payload": {string(payload)}, "revision": {revision}}, http.StatusForbidden)
	postFormJSON(t, client, portal.URL+"/admin/upstreams/config/edit", url.Values{"csrf_token": {csrf}, "payload": {string(payload)}, "revision": {revision}}, http.StatusOK)
	if len(fake.aliases["antigravity"]) != 1 || len(fake.aliases["codex"]) != 0 {
		t.Fatal("combined save mixed channel aliases")
	}
	antiCaps, _, err := keys.OAuthReasoningCaps(t.Context(), "antigravity")
	if err != nil || antiCaps["antigravity-model"] != "low" {
		t.Fatalf("caps=%v err=%v", antiCaps, err)
	}
	postFormJSON(t, client, portal.URL+"/admin/upstreams/config/edit", url.Values{"csrf_token": {csrf}, "payload": {string(payload)}, "revision": {revision}}, http.StatusConflict)
	postFormJSON(t, client, portal.URL+"/admin/upstreams/presets/new/edit", url.Values{"csrf_token": {csrf}, "payload": {string(payload)}, "revision": {"0"}, "name": {"日常"}, "action": {"save"}}, http.StatusOK)
	presets, err := st.ListOAuthPresets(t.Context())
	if err != nil || len(presets) != 1 {
		t.Fatalf("presets=%v err=%v", presets, err)
	}
	postForm(t, client, portal.URL+"/admin/upstreams/presets/"+presets[0].ID+"/apply", url.Values{"csrf_token": {csrf}}, http.StatusSeeOther)
	postForm(t, client, portal.URL+"/admin/upstreams/presets/"+presets[0].ID+"/delete", url.Values{"csrf_token": {csrf}}, http.StatusSeeOther)
	codexAccounts, err := keys.UpstreamAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	postForm(t, client, portal.URL+"/admin/upstreams/"+codexAccounts[0].ID+"/status", url.Values{"channel": {"antigravity"}, "disabled": {"true"}, "csrf_token": {csrf}}, http.StatusBadGateway)
	getBody(t, client, portal.URL+"/admin/upstreams?channel=unknown", http.StatusBadRequest)
	getBody(t, client, portal.URL+"/admin/upstreams?channel=..%2Fcodex", http.StatusBadRequest)
}
