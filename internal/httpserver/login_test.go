package httpserver

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func TestLoginRememberControlsCookieAndSession(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	p, err := st.LatestPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	user, err := accounts.Register(t.Context(), "13800138000", "用户", "long-password-123", p.Version, "")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(config.Config{CookieName: "portal_session"}, st, accounts, nil, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(s.Handler())
	defer portal.Close()
	for _, u := range []domain.User{user, admin} {
		for _, remember := range []bool{false, true} {
			jar, _ := cookiejar.New(nil)
			client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			page := getBody(t, client, portal.URL+"/login", http.StatusOK)
			csrf := extract(t, page, `name="csrf_token" value="([^"]+)"`)
			password := "long-password-123"
			if u.IsAdmin() {
				password = "very-long-admin-password"
			}
			form := url.Values{"phone": {u.Phone}, "password": {password}, "csrf_token": {csrf}}
			if remember {
				form.Set("remember", "1")
			}
			res, err := client.PostForm(portal.URL+"/login", form)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusSeeOther {
				t.Fatalf("login status=%d", res.StatusCode)
			}
			var cookie *http.Cookie
			for _, c := range res.Cookies() {
				if c.Name == s.Cfg.CookieName {
					cookie = c
				}
			}
			if cookie == nil {
				t.Fatal("missing login cookie")
			}
			wantAge, wantMax := 0, 8*time.Hour
			if remember {
				wantAge, wantMax = 30*24*60*60, 30*24*time.Hour
			}
			if cookie.MaxAge != wantAge || !cookie.Expires.IsZero() || !cookie.HttpOnly {
				t.Fatalf("cookie=%+v", cookie)
			}
			sess, err := st.Session(t.Context(), security.SHA256(cookie.Value))
			if err != nil || sess.Remember != remember || sess.ExpiresAt.Sub(sess.CreatedAt) != wantMax {
				t.Fatalf("session=%+v err=%v", sess, err)
			}
		}
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	page := getBody(t, client, portal.URL+"/login", http.StatusOK)
	csrf := extract(t, page, `name="csrf_token" value="([^"]+)"`)
	page = postForm(t, client, portal.URL+"/login", url.Values{"phone": {user.Phone}, "password": {"wrong"}, "csrf_token": {csrf}, "remember": {"1"}}, http.StatusUnauthorized)
	if !strings.Contains(page, `name="remember" value="1" checked`) {
		t.Fatal("failed login lost remember selection")
	}
}
