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
	antiDisabled bool
	configCalls  int
	configData   []byte
	excluded     map[string][]string
	aliases      map[string][]cpamp.OAuthModelAlias
}

func (f *upstreamChannelAPI) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	return []cpamp.AuthFile{{Name: "codex.json", Provider: "codex", AccountSnapshot: "codex@example.com"}, {Name: "anti.json", Provider: "antigravity", AccountSnapshot: "anti@example.com", Disabled: f.antiDisabled}}, nil
}
func (f *upstreamChannelAPI) SetAuthFileDisabled(_ context.Context, file cpamp.AuthFile, disabled bool) error {
	if file.Provider != "antigravity" {
		panic("foreign account write")
	}
	f.antiDisabled = disabled
	return nil
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
	if channel != "antigravity" {
		panic("foreign model write")
	}
	f.excluded[channel] = values
	return nil
}
func (f *upstreamChannelAPI) ListOAuthModelAliases(_ context.Context, channel string) ([]cpamp.OAuthModelAlias, error) {
	return f.aliases[channel], nil
}
func (f *upstreamChannelAPI) SetOAuthModelAliases(_ context.Context, channel string, values []cpamp.OAuthModelAlias) error {
	if channel != "antigravity" {
		panic("foreign alias write")
	}
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
	page := getBody(t, client, portal.URL+"/admin/upstreams?channel=antigravity", http.StatusOK)
	if !strings.Contains(page, "codex@example.com") || !strings.Contains(page, "codex-model") {
		t.Fatal("initial page must preload other channels")
	}
	panelBody := func(html, channel string) string {
		t.Helper()
		start := strings.Index(html, `data-upstream-channel-panel data-channel="`+channel+`"`)
		if start < 0 {
			t.Fatalf("missing channel panel: %s", channel)
		}
		body := html[start:]
		if next := strings.Index(body[1:], `data-upstream-channel-panel`); next >= 0 {
			body = body[:next+1]
		}
		return body
	}
	active := panelBody(page, "antigravity")
	if strings.Count(active, `class="upstream-quota-group"`) != 2 || strings.Count(active, `class="badge neutral">5H</span>`) != 2 || strings.Count(active, `class="badge neutral">7D</span>`) != 2 {
		t.Fatal("Antigravity page must group all four windows into two abbreviated pairs")
	}
	for _, text := range []string{"Antigravity 凭证", "anti@example.com", "antigravity-model", `name="channel" value="antigravity"`, "Gemini", "75%", `action="/quota/refresh"`, `id="reasoning-cap-form-antigravity"`} {
		if !strings.Contains(active, text) {
			t.Fatalf("page missing %q", text)
		}
	}
	for _, text := range []string{"codex@example.com", "codex-model", "暂未接入额度查询"} {
		if strings.Contains(active, text) {
			t.Fatalf("foreign UI present: %q", text)
		}
	}
	if fake.configCalls == 0 {
		t.Fatal("Antigravity reasoning settings were not read")
	}
	csrf = extract(t, page, `name="csrf_token" value="([^"]+)"`)
	post := func(path string, values url.Values) {
		t.Helper()
		values.Set("channel", "antigravity")
		values.Set("csrf_token", csrf)
		response, err := client.PostForm(portal.URL+path, values)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		target, err := url.Parse(response.Header.Get("Location"))
		if err != nil || response.StatusCode != http.StatusSeeOther || target.Query().Get("channel") != "antigravity" {
			t.Fatalf("status=%d redirect=%s err=%v", response.StatusCode, response.Header.Get("Location"), err)
		}
	}
	id := extract(t, active, `/admin/upstreams/([a-f0-9]{64})/status`)
	post("/admin/upstreams/"+id+"/status", url.Values{"disabled": {"true"}})
	if !fake.antiDisabled {
		t.Fatal("account was not disabled")
	}
	revision := extract(t, active, `name="revision" value="([^"]+)"`)
	post("/admin/upstreams/models/aliases", url.Values{"model": {"antigravity-model"}, "alias_0": {"anti-public"}, "revision": {revision}, "keep_original": {"antigravity-model"}})
	if len(fake.aliases["antigravity"]) != 1 {
		t.Fatal("alias was not saved")
	}
	// Both channels may save the same preset name.
	codexPayload, _ := json.Marshal(service.OAuthPresetSnapshot{Version: service.OAuthPresetVersion, Channel: "codex", Models: []service.OAuthPresetModel{{ID: "codex-model", Enabled: true}}})
	if _, err := st.SaveOAuthPreset(t.Context(), store.OAuthPreset{ID: "codex-preset", Name: "日常", Payload: string(codexPayload)}); err != nil {
		t.Fatal(err)
	}
	post("/admin/upstreams/presets/save", url.Values{"name": {"日常"}})
	antiPresets, err := st.ListOAuthPresets(t.Context(), "antigravity")
	if err != nil || len(antiPresets) != 1 {
		t.Fatalf("presets=%v err=%v", antiPresets, err)
	}
	// A forged foreign preset ID must not delete or apply its configuration.
	for _, action := range []string{"delete", "apply"} {
		post("/admin/upstreams/presets/codex-preset/"+action, url.Values{})
		if _, err := st.OAuthPreset(t.Context(), "codex-preset"); err != nil {
			t.Fatal("foreign preset deleted")
		}
	}
	post("/admin/upstreams/models/status", url.Values{"model": {"antigravity-model"}, "enabled": {"false"}})
	post("/admin/upstreams/presets/"+antiPresets[0].ID+"/apply", url.Values{})
	if len(fake.excluded["antigravity"]) != 0 || len(fake.aliases["antigravity"]) != 1 {
		t.Fatal("selected-channel preset not restored")
	}
	post("/admin/upstreams/presets/"+antiPresets[0].ID+"/delete", url.Values{})
	page = getBody(t, client, portal.URL+"/admin/upstreams?channel=antigravity", http.StatusOK)
	reasoningRevision := extract(t, panelBody(page, "antigravity"), `name="reasoning_revision" value="([^"]+)"`)
	post("/admin/upstreams/models/reasoning", url.Values{"model": {"antigravity-model"}, "cap_0": {"low"}, "reasoning_revision": {reasoningRevision}})
	antiCaps, _, err := keys.OAuthReasoningCaps(t.Context(), "antigravity")
	if err != nil || antiCaps["antigravity-model"] != "low" {
		t.Fatalf("caps=%v err=%v", antiCaps, err)
	}
	codexCaps, _, err := keys.OAuthReasoningCaps(t.Context())
	if err != nil || len(codexCaps) != 0 {
		t.Fatalf("foreign caps=%v err=%v", codexCaps, err)
	}
	postForm(t, client, portal.URL+"/admin/upstreams/models/reasoning", url.Values{"channel": {"antigravity"}, "csrf_token": {csrf}}, http.StatusSeeOther)
	getBody(t, client, portal.URL+"/admin/upstreams?channel=unknown", http.StatusBadRequest)
	getBody(t, client, portal.URL+"/admin/upstreams?channel=..%2Fcodex", http.StatusBadRequest)
	codexAccounts, err := keys.UpstreamAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	postForm(t, client, portal.URL+"/admin/upstreams/"+codexAccounts[0].ID+"/status", url.Values{"channel": {"antigravity"}, "disabled": {"true"}, "csrf_token": {csrf}}, http.StatusBadGateway)
	if len(fake.excluded["codex"]) != 0 || len(fake.aliases["codex"]) != 0 {
		t.Fatal("Codex state changed")
	}
	request, _ := http.NewRequest(http.MethodGet, portal.URL+"/admin/upstreams?channel=antigravity", nil)
	request.Header.Set("X-Upstream-Channel-Only", "true")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || strings.Contains(string(data), `data-upstream-channel-panel data-channel="codex"`) || !strings.Contains(string(data), `data-upstream-channel-panel data-channel="antigravity"`) {
		t.Fatal("quota polling must only load its original channel")
	}
}
