package httpserver

import (
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func TestSharedUpstreamLayoutsAcrossAdminSessionsAndScopes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	password := "very-long-admin-password"
	first, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员甲", password, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := accounts.CreateAdmin(t.Context(), "13800138000", "管理员乙", password, &first, "")
	if err != nil {
		t.Fatal(err)
	}
	fake := &upstreamChannelAPI{excluded: map[string][]string{}, aliases: map[string][]cpamp.OAuthModelAlias{}}
	s, err := New(config.Config{CookieName: "portal_session", TimeZone: time.UTC}, st, accounts, service.NewKeys(st, fake), secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(s.Handler())
	defer portal.Close()
	login := func(phone string) (*http.Client, string) {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		page := getBody(t, client, portal.URL+"/login", http.StatusOK)
		csrf := extract(t, page, `name="csrf_token" value="([^"]+)"`)
		postForm(t, client, portal.URL+"/login", url.Values{"csrf_token": {csrf}, "phone": {phone}, "password": {password}}, http.StatusSeeOther)
		page = getBody(t, client, portal.URL+"/admin/upstreams", http.StatusOK)
		return client, extract(t, page, `name="csrf_token" value="([^"]+)"`)
	}
	one, csrfOne := login(first.Phone)
	two, csrfTwo := login(second.Phone)
	for _, id := range []string{"preset-a", "preset-b"} {
		if _, err := st.SaveOAuthPreset(t.Context(), store.OAuthPreset{ID: id, Name: id, Payload: `{"version":3,"channels":[{"channel":"codex","models":[]}]}`}); err != nil {
			t.Fatal(err)
		}
	}
	save := func(client *http.Client, csrf, scope, order string, code int) string {
		return postFormJSON(t, client, portal.URL+"/admin/upstreams/layout", url.Values{"csrf_token": {csrf}, "scope": {scope}, "order": {order}}, code)
	}
	callsBeforeSave := fake.configCalls
	if body := save(one, csrfOne, "presets", `["preset-b","preset-a"]`, http.StatusOK); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("save reply=%s", body)
	}
	save(one, csrfOne, "aliases:codex", `["codex-model"]`, http.StatusOK)
	save(one, csrfOne, "aliases:antigravity", `["antigravity-model"]`, http.StatusOK)
	if fake.configCalls != callsBeforeSave || len(fake.configData) != 0 || len(fake.aliases) != 0 || len(fake.excluded) != 0 {
		t.Fatal("layout saving must not read or write upstream model configuration")
	}
	page := html.UnescapeString(getBody(t, two, portal.URL+"/admin/upstreams?channel=antigravity", http.StatusOK))
	for _, want := range []string{
		`data-masonry-key="presets" data-masonry-order="["preset-b","preset-a"]"`,
		`data-masonry-key="aliases:codex" data-masonry-order="["codex-model"]"`,
		`data-masonry-key="aliases:antigravity" data-masonry-order="["antigravity-model"]"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("second browser missing shared layout %s", want)
		}
	}
	save(two, csrfTwo, "presets", `["preset-a","preset-b"]`, http.StatusOK)
	order, err := st.UpstreamCardOrder(t.Context(), "presets")
	if err != nil || !reflect.DeepEqual(order, []string{"preset-a", "preset-b"}) {
		t.Fatalf("latest administrator order=%v err=%v", order, err)
	}
	page = html.UnescapeString(getBody(t, one, portal.URL+"/admin/upstreams", http.StatusOK))
	if !strings.Contains(page, `data-masonry-order="["preset-a","preset-b"]"`) {
		t.Fatal("first browser did not load the latest shared order")
	}
	for _, invalid := range []struct{ scope, order string }{
		{"registration_open", `["x"]`}, {"aliases:", `[]`}, {"presets", `["x","x"]`},
		{"presets", `null`}, {"presets", `{}`}, {"presets", `bad`}, {"presets", `[""]`},
	} {
		save(one, csrfOne, invalid.scope, invalid.order, http.StatusBadRequest)
	}
	save(one, "invalid-csrf", "presets", `[]`, http.StatusForbidden)
	save(two, csrfOne, "presets", `[]`, http.StatusForbidden)
	save(one, csrfOne, "presets", strings.Repeat(" ", 129<<10), http.StatusBadRequest)
	order, err = st.UpstreamCardOrder(t.Context(), "presets")
	if err != nil || !reflect.DeepEqual(order, []string{"preset-a", "preset-b"}) {
		t.Fatal("invalid saves changed shared layout")
	}
	if err := st.SetUserRole(t.Context(), second.ID, domain.RoleUser); err != nil {
		t.Fatal(err)
	}
	save(two, csrfTwo, "presets", `[]`, http.StatusForbidden)
	anonymous := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	save(anonymous, csrfOne, "presets", `[]`, http.StatusSeeOther)
}
