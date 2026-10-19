package httpserver

import (
	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceAdminAccessSaveAndTotalBudgets(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "portal.db")
	state := filepath.Join(dir, "resources")
	st, e := store.Open(db, true)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	password := "resource-admin-test-password"
	admin, e := accounts.CreateAdmin(t.Context(), "13900139000", "资源管理员", password, nil, "")
	if e != nil {
		t.Fatal(e)
	}
	user := domain.User{ID: "ordinary", Phone: "13800138000", Name: "普通用户", PasswordHash: admin.PasswordHash, Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if e = st.CreateUser(t.Context(), user); e != nil {
		t.Fatal(e)
	}
	s, e := New(config.Config{DatabasePath: db, BackupStateDir: filepath.Join(dir, "backup"), CookieName: "portal_session", TimeZone: time.UTC}, st, accounts, service.NewKeys(st, &backupTestAPI{}), secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	login := func(phone string) (*http.Client, string) {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		page := getBody(t, c, server.URL+"/login", 200)
		csrf := extract(t, page, `name="csrf_token" value="([^"]+)"`)
		postForm(t, c, server.URL+"/login", url.Values{"csrf_token": {csrf}, "phone": {phone}, "password": {password}}, 303)
		return c, csrf
	}
	anonymous := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	getBody(t, anonymous, server.URL+"/admin/system/resources/status", 303)
	ordinary, _ := login(user.Phone)
	getBody(t, ordinary, server.URL+"/admin/system/resources/status", 403)
	postForm(t, ordinary, server.URL+"/admin/system/resources/save", url.Values{}, 403)
	client, _ := login(admin.Phone)
	page := getBody(t, client, server.URL+"/admin/system", 200)
	csrf := extract(t, page, `name="csrf_token" value="([^"]+)"`)
	if !strings.Contains(page, "观察模式") || !strings.Contains(page, "资源保护") {
		t.Fatal("default panel missing")
	}
	if _, e = os.Stat(state); !os.IsNotExist(e) {
		t.Fatal("read-only page created resource state")
	}
	for _, action := range []string{"save"} {
		postForm(t, client, server.URL+"/admin/system/resources/"+action, url.Values{"csrf_token": {"wrong"}, "admin_password": {password}}, 403)
	}
	form := url.Values{"csrf_token": {csrf}, "revision": {"1"}, "mode": {"enforce"}, "global": {"2"}, "per_user": {"1"}, "body_mib": {"10"}, "cpu_percent": {"90"}, "memory_mib": {"768"}}
	form["global"] = []string{"2", "3"}
	postForm(t, client, server.URL+"/admin/system/resources/save", form, 400)
	form.Set("global", "2")
	s.limit = newLimiter()
	postForm(t, client, server.URL+"/admin/system/resources/save", form, 303)
	p, e := st.ResourcePolicy(t.Context())
	if e != nil || p.Revision != 2 || p.Mode != "enforce" || s.Resources.Policy().Mode != "enforce" {
		t.Fatal("save not active", p, e)
	}
	if _, e = os.Stat(state); !os.IsNotExist(e) {
		t.Fatal("settings save created host state")
	}
	form.Set("global", "3")
	postForm(t, client, server.URL+"/admin/system/resources/save", form, 303)
	p, _ = st.ResourcePolicy(t.Context())
	if p.Global != 2 {
		t.Fatal("stale editor overwrote settings")
	}
	m := resourcecontrol.Metrics{At: time.Now(), HostID: "host-test", CPUCount: 1, CPUPercent: 12.3, TotalMiB: 926, AvailableMiB: 410}
	s.Resources.UpdateMetrics(m, time.Now())
	form.Set("revision", "2")
	form.Set("memory_mib", "926")
	postForm(t, client, server.URL+"/admin/system/resources/save", form, 303)
	p, _ = st.ResourcePolicy(t.Context())
	if p.Revision != 2 || p.MemoryMiB != 768 {
		t.Fatal("impossible total budget saved")
	}
	page = getBody(t, client, server.URL+"/admin/system", 200)
	if strings.Contains(page, "最近保护记录") || strings.Contains(page, "流式请求和 WebSocket 连接持续计入并发") || strings.Contains(page, "resource-save-password") || strings.Contains(page, "应用容器预算") || strings.Contains(page, "portal_memory") || strings.Contains(page, "cpa_memory") || !strings.Contains(page, "已用 / 总内存") || !strings.Contains(page, "516 / 926 MiB") {
		t.Fatal("panel is not total-only")
	}
	// Values shown in the table are escaped, including user-controlled names.
	s.Resources.Record("<script>bad</script>", "request_too_large")
	s.Resources.FlushEvents(t.Context())
	page = getBody(t, client, server.URL+"/admin/system", 200)
	if strings.Contains(page, "<script>bad</script>") || !strings.Contains(page, "&lt;script&gt;bad&lt;/script&gt;") || !strings.Contains(page, "资源保护拦截") {
		t.Fatal("unsafe event rendering")
	}
	for _, field := range []string{"global", "per_user", "body_mib", "cpu_percent", "memory_mib"} {
		form.Set(field, "0")
	}
	postForm(t, client, server.URL+"/admin/system/resources/save", form, 303)
	saved, e := st.ResourcePolicy(t.Context())
	if e != nil || saved.Global != 0 || saved.PerUser != 0 || saved.BodyMiB != 0 || saved.CPUPercent != 0 || saved.MemoryMiB != 0 {
		t.Fatal("zero limits did not persist", saved, e)
	}
	if s.resourceView().Active != "0 / 不限" {
		t.Fatal("unlimited concurrency displayed as zero capacity")
	}
	response, e := client.Get(server.URL + "/admin/system/resources/status")
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.Header.Get("Cache-Control") != "no-store" || !json.Valid(body) || strings.Contains(string(body), password) || strings.Contains(string(body), admin.PasswordHash) {
		t.Fatal("unsafe status response")
	}
	// Optional browser-review fixture; contains synthetic test data only.
	if out := os.Getenv("PORTAL_TEST_UI_DIR"); out != "" {
		if e = os.MkdirAll(filepath.Join(out, "static"), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(out, "index.html"), []byte(page), 0600); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(out, "status.json"), body, 0600); e != nil {
			t.Fatal(e)
		}
		entries, _ := fs.ReadDir(webui.Assets(), ".")
		for _, entry := range entries {
			if !entry.IsDir() {
				b, e := fs.ReadFile(webui.Assets(), entry.Name())
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(out, "static", entry.Name()), b, 0600); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
}
