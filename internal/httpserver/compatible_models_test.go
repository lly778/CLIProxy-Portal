package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"gopkg.in/yaml.v3"
)

type compatibleEditorAPI struct {
	presetEditorAPI
	failConfigWrite bool
}

func (f *compatibleEditorAPI) PutConfigYAML(ctx context.Context, data []byte) error {
	if f.failConfigWrite {
		f.failConfigWrite = false
		return errors.New("fixture config write failed")
	}
	return f.presetEditorAPI.PutConfigYAML(ctx, data)
}

func TestCompatibleProviderInRoutingEditorAndPresets(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	admin, err := accounts.CreateAdmin(ctx, "13900139000", "Admin", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	token, csrf, err := accounts.CreateSession(ctx, admin, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	upstream := &upstreamChannelAPI{
		excluded: map[string][]string{}, aliases: map[string][]cpamp.OAuthModelAlias{},
		configData: []byte(`openai-compatibility:
  - name: DeepSeek
    base-url: https://api.deepseek.com/v1
    api-key-entries:
      - api-key: fixture-upstream-secret
        proxy-url: direct
    headers:
      X-Test: keep
    models:
      - name: deepseek-v4-pro
        alias: pro-original
      - name: deepseek-flash
        alias: deepseek-flash
        max-context-length: 1000000
`),
	}
	fake := &compatibleEditorAPI{presetEditorAPI: presetEditorAPI{upstreamChannelAPI: upstream}}
	keys := service.NewKeys(st, fake)
	s, err := New(config.Config{CookieName: "portal_session", TimeZone: time.UTC}, st, accounts, keys, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "portal_session", Value: token})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if strings.Contains(w.Body.String(), "fixture-upstream-secret") {
			t.Fatal("upstream credential reached the browser")
		}
		return w
	}
	modelSummary := func() string {
		t.Helper()
		w := request(http.MethodGet, "/admin/upstreams", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("summary page: %d %s", w.Code, w.Body)
		}
		summary, _, found := strings.Cut(w.Body.String(), "upstream-saved-presets-head")
		if !found {
			t.Fatal("upstream model summary section is missing")
		}
		return summary
	}
	if summary := modelSummary(); !strings.Contains(summary, "<h3>DeepSeek（第三方）</h3>") || !strings.Contains(summary, "deepseek-flash") || !strings.Contains(summary, "pro-original") {
		t.Fatal("enabled third-party models are missing from the summary before any preset is saved")
	}
	const edit = "/admin/upstreams/config/edit"
	w := request(http.MethodGet, edit, nil)
	var editor struct {
		Revision string
		Channels []presetEditorChannel
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &editor) != nil {
		t.Fatalf("editor load failed: %d %s", w.Code, w.Body)
	}
	var channel string
	for _, group := range editor.Channels {
		if group.Label != "DeepSeek（第三方）" {
			continue
		}
		channel = group.Channel
		if len(group.Models) != 2 {
			t.Fatalf("missing DeepSeek models: %+v", group)
		}
		for _, model := range group.Models {
			if !model.Enabled || !reflect.DeepEqual(model.ReasoningLevels, []string{"low", "high", "max"}) {
				t.Fatalf("wrong live model state: %+v", model)
			}
		}
	}
	if channel == "" {
		t.Fatal("missing third-party channel")
	}
	before, err := keys.OAuthPresetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edited := service.OAuthChannelPreset{
		Channel:       channel,
		Models:        []service.OAuthPresetModel{{ID: "deepseek-v4-pro", Enabled: true}, {ID: "deepseek-flash"}},
		Aliases:       []cpamp.OAuthModelAlias{{Name: "deepseek-v4-pro", Alias: "my-deepseek", Fork: true}},
		ReasoningCaps: map[string]string{"deepseek-v4-pro": "high"},
	}
	target := service.OAuthPresetSnapshot{Version: service.OAuthPresetVersion, Channels: []service.OAuthChannelPreset{edited}}
	data, _ := json.Marshal(target)
	w = request(http.MethodPost, "/admin/upstreams/presets/new/edit", url.Values{
		"csrf_token": {csrf}, "payload": {string(data)}, "name": {"DeepSeek preset"}, "action": {"save"},
	})
	if w.Code != http.StatusOK || fake.writes != 0 {
		t.Fatalf("save draft: %d %s", w.Code, w.Body)
	}
	channelEdit := edit + "?channel=" + channel
	original, err := keys.OAuthChannelConfiguration(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	w = request(http.MethodPost, channelEdit, url.Values{
		"csrf_token": {csrf}, "payload": {string(data)}, "revision": {service.OAuthConfigurationRevision(original)},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(string(fake.configData), "protocol: openai") || !strings.Contains(string(fake.configData), "reasoning_effort: max") {
		t.Fatalf("missing translated effort cap")
	}
	if summary := modelSummary(); !strings.Contains(summary, "my-deepseek") || !strings.Contains(summary, "上限 high") || strings.Contains(summary, "deepseek-flash") {
		t.Fatal("summary did not reflect the enabled model, alias and cap")
	}
	after, err := service.NewKeys(st, fake).OAuthChannelConfiguration(ctx, channel)
	if err != nil || !service.OAuthPresetSnapshotsEqual(target, after) {
		t.Fatalf("disabled model or alias lost across reads: %+v %v", after, err)
	}
	fake.failConfigWrite = true
	if err := keys.ApplyOAuthPreset(ctx, before); err == nil {
		t.Fatal("failed config write reported success")
	}
	afterFailure, err := keys.OAuthChannelConfiguration(ctx, channel)
	if err != nil || !service.OAuthPresetSnapshotsEqual(target, afterFailure) {
		t.Fatalf("failed re-enable lost a disabled model: %+v %v", afterFailure, err)
	}
	allOff := service.OAuthPresetSnapshot{Version: service.OAuthPresetVersion, Channels: []service.OAuthChannelPreset{{
		Channel: channel, Models: []service.OAuthPresetModel{{ID: "deepseek-v4-pro"}, {ID: "deepseek-flash"}},
	}}}
	if err := keys.ApplyOAuthPreset(ctx, allOff); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fake.configData), "disabled: true") || !strings.Contains(string(fake.configData), "my-deepseek") {
		t.Fatal("all-off must disable the provider and retain its model mappings")
	}
	if strings.Contains(modelSummary(), "<h3>DeepSeek（第三方）</h3>") {
		t.Fatal("disabled provider is still listed in the enabled-model summary")
	}
	if err := keys.ApplyOAuthPreset(ctx, before); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fake.configData), "reasoning_effort:") {
		t.Fatal("restoring preset left a stale cap")
	}
	var actual struct {
		Providers []struct {
			Keys    []map[string]string `yaml:"api-key-entries"`
			Headers map[string]string
			Models  []map[string]any
		} `yaml:"openai-compatibility"`
	}
	if err := yaml.Unmarshal(fake.configData, &actual); err != nil || len(actual.Providers) != 1 {
		t.Fatalf("invalid provider configuration: %v", err)
	}
	p := actual.Providers[0]
	if !reflect.DeepEqual(p.Keys, []map[string]string{{"api-key": "fixture-upstream-secret", "proxy-url": "direct"}}) || p.Headers["X-Test"] != "keep" {
		t.Fatal("credential settings changed while applying model rules")
	}
	for _, model := range p.Models {
		if model["name"] == "deepseek-flash" && model["max-context-length"] == 1000000 {
			return
		}
	}
	t.Fatal("restored model lost its original metadata")
}
