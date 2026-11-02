package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

type clientModelsAPI struct {
	cpamp.API
	modelReads int
}

func (f *clientModelsAPI) ListAPIKeys(context.Context) ([]string, error) {
	return []string{"own-key", "other-key"}, nil
}
func (f *clientModelsAPI) ListModelsWithAPIKey(_ context.Context, key string) (cpamp.ModelListResponse, error) {
	f.modelReads++
	if key != "own-key" {
		panic("queried another user's key")
	}
	return cpamp.ModelListResponse{Data: []cpamp.Model{{ID: "public"}}}, nil
}
func (f *clientModelsAPI) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) { return nil, nil }
func (f *clientModelsAPI) ListOAuthModelAliases(context.Context, string) ([]cpamp.OAuthModelAlias, error) {
	return nil, nil
}

func (f *clientModelsAPI) ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error) {
	return []cpamp.OAuthModelDefinition{{ID: "public", ThinkingLevels: []string{"low", "medium", "high"}}, {ID: "offline", ThinkingLevels: []string{"low", "medium", "high", "xhigh"}}, {ID: "disabled"}}, nil
}

func TestClientModelsPublicReadAndAdminOnlySave(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	upstream := &clientModelsAPI{}
	keys := service.NewKeys(st, upstream)
	s, err := New(config.Config{CookieName: "portal_session", CPAAPIBaseURL: "https://api.example.test"}, st, accounts, keys, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := domain.User{ID: "user", Phone: "13800138000", Name: "User", Status: domain.StatusPending, Role: domain.RoleUser, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	token, csrf, err := accounts.CreateSession(ctx, u, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/client-model-config", nil)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "portal_session", Value: token})
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request(""); w.Code != 404 {
		t.Fatalf("unconfigured=%d", w.Code)
	}
	if w := request(token); w.Code != 404 {
		t.Fatalf("pending without config=%d", w.Code)
	}
	if err = st.SetUserStatus(ctx, u.ID, domain.StatusApproved, ""); err != nil {
		t.Fatal(err)
	}
	if w := request(token); w.Code != 404 {
		t.Fatalf("unpublished=%d body=%s", w.Code, w.Body)
	}
	if upstream.modelReads != 0 {
		t.Fatal("queried models without permission/publication")
	}
	c := domain.ClientModels{Version: domain.ClientModelsVersion, Enabled: true, DefaultModel: "offline", Models: []domain.ClientModel{{ID: "offline", DefaultReasoning: "high"}, {ID: "public", DefaultReasoning: "low"}}}
	if err = st.SaveClientModels(ctx, c, 0); err != nil {
		t.Fatal(err)
	}
	w := request("")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-CSRF-Token") != "" {
		t.Fatalf("response=%d headers=%v body=%s", w.Code, w.Header(), w.Body)
	}
	var got domain.ClientModels
	if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.DefaultModel != "offline" || len(got.Models) != 2 || got.Models[0].DefaultReasoning != "high" || got.APIBaseURL != "https://api.example.test/v1" {
		t.Fatalf("publication=%+v", got)
	}
	for _, secret := range []string{"own-key", "other-key", u.Phone, `"reasoning_levels":`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("configuration leaked unauthorized information")
		}
	}
	if strings.Join(got.Models[0].SupportedReasoningLevels, ",") != "low,medium,high,xhigh" {
		t.Fatalf("lost declared levels: %+v", got.Models[0])
	}
	readsBeforeRepeat := upstream.modelReads
	if w := request(""); w.Code != 200 || upstream.modelReads != readsBeforeRepeat {
		t.Fatal("repeat configuration read bypassed shared catalog cache")
	}
	// A normal user cannot publish, even with a valid login and CSRF token.
	r := httptest.NewRequest(http.MethodPost, "/admin/upstreams/client-models", strings.NewReader(url.Values{"csrf_token": {csrf}, "client_revision": {"1"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "portal_session", Value: token})
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin save=%d", w.Code)
	}
	admin := domain.User{ID: "admin", Phone: "13900139000", Name: "Admin", Role: domain.RoleAdmin, Status: domain.StatusApproved, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = st.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	adminToken, adminCSRF, err := accounts.CreateSession(ctx, admin, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"client_revision": {"1"}, "default_model": {"public"}, "client_model_id": {"public"}, "include_model": {"public"}, "default_0": {"high"}}
	save := func() int {
		r := httptest.NewRequest(http.MethodPost, "/admin/upstreams/client-models", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "portal_session", Value: adminToken})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if code := save(); code != http.StatusForbidden {
		t.Fatalf("missing CSRF=%d", code)
	}
	form.Set("csrf_token", adminCSRF)
	if code := save(); code != http.StatusSeeOther {
		t.Fatalf("admin publish=%d", code)
	}
	if code := save(); code != http.StatusConflict {
		t.Fatalf("stale admin form=%d", code)
	}
	form.Set("client_revision", "2")
	form.Set("default_0", "unsupported")
	if code := save(); code != http.StatusBadRequest {
		t.Fatalf("invalid reasoning default=%d", code)
	}
	stored, err := st.ClientModels(ctx)
	if err != nil || stored.Revision != 2 || stored.Models[0].DefaultReasoning != "high" {
		t.Fatalf("publication overwritten: %+v err=%v", stored, err)
	}
	if err = st.SetUserStatus(ctx, u.ID, domain.StatusSuspended, ""); err != nil {
		t.Fatal(err)
	}
	reads := upstream.modelReads
	if w := request(token); w.Code != http.StatusOK {
		t.Fatalf("suspended=%d", w.Code)
	}
	if reads != upstream.modelReads {
		t.Fatal("public configuration fetch queried upstream")
	}
}
