package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

type presetEditorAPI struct {
	*upstreamChannelAPI
	writes   int
	failNext bool
}

func (f *presetEditorAPI) SetOAuthModelAliases(ctx context.Context, channel string, aliases []cpamp.OAuthModelAlias) error {
	f.writes++
	if f.failNext {
		f.failNext = false
		return errors.New("test apply failure")
	}
	return f.upstreamChannelAPI.SetOAuthModelAliases(ctx, channel, aliases)
}
func (f *presetEditorAPI) SetOAuthExcludedModels(ctx context.Context, channel string, models []string) error {
	f.writes++
	return f.upstreamChannelAPI.SetOAuthExcludedModels(ctx, channel, models)
}
func (f *presetEditorAPI) PutConfigYAML(ctx context.Context, data []byte) error {
	f.writes++
	return f.upstreamChannelAPI.PutConfigYAML(ctx, data)
}

func TestPresetEditorSavesDraftBeforeApplyAndPreservesErrors(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	admin, err := accounts.CreateAdmin(t.Context(), "13900139000", "Admin", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	fake := &presetEditorAPI{upstreamChannelAPI: &upstreamChannelAPI{excluded: map[string][]string{}, aliases: map[string][]cpamp.OAuthModelAlias{}}}
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
	csrf = extract(t, page, `name="csrf_token" value="([^"]+)"`)
	original, err := keys.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(original)
	var savedDraft service.OAuthPresetSnapshot
	_ = json.Unmarshal(encoded, &savedDraft)
	for i := range savedDraft.Channels {
		if savedDraft.Channels[i].Channel == "codex" {
			savedDraft.Channels[i].Models[0].Enabled = false
		}
	}
	encoded, _ = json.Marshal(savedDraft)
	stored, err := st.SaveOAuthPreset(t.Context(), store.OAuthPreset{ID: "editor", Name: "Draft", Payload: string(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := portal.URL + "/admin/upstreams/presets/editor/edit"
	data := getBody(t, client, endpoint, http.StatusOK)
	var editor struct {
		Name, Revision string
		Channels       []presetEditorChannel
	}
	if err := json.Unmarshal([]byte(data), &editor); err != nil {
		t.Fatal(err)
	}
	if editor.Name != "Draft" || len(editor.Channels) != 2 {
		t.Fatalf("editor data: %s", data)
	}
	for _, channel := range editor.Channels {
		if channel.Channel == "codex" && channel.Models[0].Enabled {
			t.Fatal("editor used live state instead of saved state")
		}
	}
	page = getBody(t, client, portal.URL+"/admin/upstreams", http.StatusOK)
	if !strings.Contains(page, `data-preset-edit="/admin/upstreams/presets/editor/edit"`) || !strings.Contains(page, "保存并应用") {
		t.Fatal("missing editor trigger or action")
	}
	post := func(name, revision, action, payload, token string, status int) map[string]any {
		t.Helper()
		resp, err := client.PostForm(endpoint, url.Values{"name": {name}, "revision": {revision}, "action": {action}, "payload": {payload}, "csrf_token": {token}, "channel": {"antigravity"}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("status=%d want=%d reply=%v", resp.StatusCode, status, reply)
		}
		return reply
	}
	var target service.OAuthPresetSnapshot
	encoded, _ = json.Marshal(original)
	_ = json.Unmarshal(encoded, &target)
	for i := range target.Channels {
		entry := &target.Channels[i]
		if entry.Channel == "codex" {
			entry.Aliases = []cpamp.OAuthModelAlias{{Name: "codex-model", Alias: "draft-public", Fork: true}}
			entry.ReasoningCaps = map[string]string{"codex-model": "high"}
		}
		if entry.Channel == "antigravity" {
			entry.Models[0].Enabled = false
			entry.Aliases = nil
			entry.ReasoningCaps = nil
		}
	}
	encoded, _ = json.Marshal(target)
	post("Renamed", editor.Revision, "save", string(encoded), "invalid", 403)
	reply := post("Renamed", editor.Revision, "save", string(encoded), csrf, 200)
	if !strings.Contains(reply["redirect"].(string), "channel=antigravity") || fake.writes != 0 {
		t.Fatalf("save changed live configuration: %v writes=%d", reply, fake.writes)
	}
	updated, err := st.OAuthPreset(t.Context(), stored.ID)
	if err != nil || updated.Name != "Renamed" || updated.ID != stored.ID {
		t.Fatalf("rename: %+v %v", updated, err)
	}
	live, err := keys.OAuthPresetSnapshot(t.Context())
	if err != nil || !service.OAuthPresetSnapshotsEqual(original, live) {
		t.Fatalf("save applied live config: %+v %v", live, err)
	}
	post("Lost update", editor.Revision, "save", string(encoded), csrf, 409)
	invalid := strings.Replace(string(encoded), "draft-public", "bad alias", 1)
	post("Invalid", strconv.FormatInt(updated.UpdatedAt.UnixMilli(), 10), "save", invalid, csrf, 400)
	if fake.writes != 0 {
		t.Fatal("rejected draft changed live state")
	}
	post("Renamed", strconv.FormatInt(updated.UpdatedAt.UnixMilli(), 10), "apply", string(encoded), csrf, 200)
	live, err = keys.OAuthPresetSnapshot(t.Context())
	if err != nil || !service.OAuthPresetSnapshotsEqual(target, live) {
		t.Fatalf("save/apply target mismatch: %+v %v", live, err)
	}
	updated, _ = st.OAuthPreset(t.Context(), stored.ID)
	for i := range target.Channels {
		if target.Channels[i].Channel == "codex" {
			target.Channels[i].Models[0].Enabled = false
			target.Channels[i].Aliases = nil
			target.Channels[i].ReasoningCaps = nil
		}
	}
	encoded, _ = json.Marshal(target)
	fake.failNext = true
	reply = post("Retryable", strconv.FormatInt(updated.UpdatedAt.UnixMilli(), 10), "apply", string(encoded), csrf, 502)
	if reply["saved"] != true || reply["revision"] == nil || !strings.Contains(reply["error"].(string), "预设已保存") {
		t.Fatalf("partial success hidden: %v", reply)
	}
	failedSave, _ := st.OAuthPreset(t.Context(), stored.ID)
	if failedSave.Name != "Retryable" || failedSave.Payload != string(encoded) {
		t.Fatal("save before apply lost edited draft")
	}
	restored, err := keys.OAuthPresetSnapshot(t.Context())
	if err != nil || !service.OAuthPresetSnapshotsEqual(live, restored) {
		t.Fatalf("failed apply lost live configuration: %+v %v", restored, err)
	}
	anonymous := &http.Client{CheckRedirect: client.CheckRedirect}
	getBody(t, anonymous, endpoint, http.StatusSeeOther)

	endpoint = portal.URL + "/admin/upstreams/presets/new/edit"
	writes := fake.writes
	data = getBody(t, client, endpoint, http.StatusOK)
	var fresh struct {
		Name, Revision string
		Creating       bool
		Channels       []presetEditorChannel
	}
	if err := json.Unmarshal([]byte(data), &fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh.Creating || fresh.Name != "" || fresh.Revision != "0" || len(fresh.Channels) != 2 || fake.writes != writes {
		t.Fatalf("new editor changed live config: %s writes=%d", data, fake.writes)
	}
	encoded, _ = json.Marshal(live)
	post("New draft", "0", "save", string(encoded), csrf, 200)
	if fake.writes != writes {
		t.Fatal("new save-only preset changed live configuration")
	}
	items, err := st.ListOAuthPresets(t.Context())
	if err != nil || len(items) != 2 {
		t.Fatalf("new preset not created: %+v %v", items, err)
	}
	post("NEW DRAFT", "0", "save", string(encoded), csrf, 409)
	items, _ = st.ListOAuthPresets(t.Context())
	if len(items) != 2 {
		t.Fatal("duplicate new name created a preset")
	}
	fake.failNext = true
	encoded, _ = json.Marshal(target)
	reply = post("New and apply", "0", "apply", string(encoded), csrf, 502)
	if reply["saved"] != true || reply["edit_url"] == nil || !strings.HasPrefix(reply["edit_url"].(string), "/admin/upstreams/presets/preset_") {
		t.Fatalf("failed create/apply lost retry URL: %v", reply)
	}
	items, _ = st.ListOAuthPresets(t.Context())
	for len(items) < store.MaxOAuthPresets {
		_, err = st.CreateOAuthPreset(t.Context(), store.OAuthPreset{ID: fmt.Sprintf("limit-%d", len(items)), Name: fmt.Sprintf("Limit %d", len(items)), Payload: string(encoded)})
		if err != nil {
			t.Fatal(err)
		}
		items, _ = st.ListOAuthPresets(t.Context())
	}
	getBody(t, client, endpoint, http.StatusConflict)
	post("Over limit", "0", "save", string(encoded), csrf, 409)
}
