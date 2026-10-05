package httpserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/backup"
	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

type backupTestAPI struct{ cpamp.API }

func (*backupTestAPI) Status(context.Context) (cpamp.StatusResponse, error) {
	return cpamp.StatusResponse{}, errors.New("not measured")
}
func (*backupTestAPI) Health(context.Context) (cpamp.HealthResponse, error) {
	return cpamp.HealthResponse{}, errors.New("not measured")
}
func (*backupTestAPI) ListAPIKeys(context.Context) ([]string, error)            { return nil, nil }
func (*backupTestAPI) ListAliases(context.Context) ([]cpamp.APIKeyAlias, error) { return nil, nil }
func (*backupTestAPI) ReplaceAliases(_ context.Context, rows []cpamp.APIKeyAlias, _ []string, _ bool) ([]cpamp.APIKeyAlias, error) {
	return rows, nil
}
func (*backupTestAPI) Analytics(context.Context, cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	return cpamp.AnalyticsResponse{}, nil
}

func TestBackupHostConfigurationAndAdminOperations(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "backup")
	dbFile := filepath.Join(dir, "portal.db")
	st, err := store.Open(dbFile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	accounts := service.NewAccounts(st, secret)
	password := "very-long-admin-password"
	admin, err := accounts.CreateAdmin(t.Context(), "13900139000", "备份管理员", password, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	fake := &backupTestAPI{}
	s, err := New(config.Config{DatabasePath: dbFile, BackupStateDir: state, CookieName: "portal_session", TimeZone: time.UTC, ReconcileInterval: time.Minute}, st, accounts, service.NewKeys(st, fake), secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(s.Handler())
	defer portal.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	page := getBody(t, client, portal.URL+"/login", 200)
	csrf := extract(t, page, `name="csrf_token" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/login", url.Values{"csrf_token": {csrf}, "phone": {admin.Phone}, "password": {password}}, 303)
	page = getBody(t, client, portal.URL+"/admin/system", 200)
	csrf = extract(t, page, `name="csrf_token" value="([^"]+)"`)
	if _, err = os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("GET initialized backup state")
	}
	base := url.Values{"csrf_token": {csrf}, "admin_password": {password}}
	for _, path := range []string{"settings", "key"} {
		postForm(t, client, portal.URL+"/admin/system/backup/"+path, url.Values{"csrf_token": {"wrong"}, "admin_password": {password}}, 403)
	}
	postForm(t, client, portal.URL+"/admin/system/backup/run", base, 400)
	key, err := backup.EnsureKey(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = backup.SaveToken(state, "test-only-github-secret-token"); err != nil {
		t.Fatal(err)
	}
	c := backup.Config{Repository: "owner/private", Weekday: 5, Hour: 7, Minute: 35, Retain: 6, Enabled: false, KeySaved: true, Instance: "0123456789abcdef"}
	if err = backup.SaveConfig(state, c); err != nil {
		t.Fatal(err)
	}
	page = getBody(t, client, portal.URL+"/admin/system", 200)
	for _, hidden := range []string{"test-only-github-secret-token", "定时与上传设置", "name=\"retain\""} {
		if strings.Contains(page, hidden) {
			t.Fatalf("page exposed backend configuration %q", hidden)
		}
	}
	for _, want := range []string{`value="owner/private"`, `name="github_token"`, `下载备份恢复密钥`, `留空保持不变`, `type="time" name="backup_time" value="07:35"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("missing setting %q", want)
		}
	}
	s.limit = newLimiter()
	settings := url.Values{"csrf_token": {csrf}, "admin_password": {password}, "repository": {"owner/private"}, "github_token": {""}, "key_saved": {"1"}, "weekday": {"5"}, "backup_time": {"07:35"}}
	settings.Set("admin_password", "wrong")
	postForm(t, client, portal.URL+"/admin/system/backup/settings", settings, 403)
	settings.Set("admin_password", password)
	settings.Set("retain", "10")
	postForm(t, client, portal.URL+"/admin/system/backup/settings", settings, 400)
	settings.Del("retain")
	settings.Del("key_saved")
	postForm(t, client, portal.URL+"/admin/system/backup/settings", settings, 400)
	settings.Set("key_saved", "1")
	for _, invalid := range []string{"24:00", "07:60", "07:35:00", "", "7:35", "12"} {
		s.limit = newLimiter()
		settings.Set("backup_time", invalid)
		postForm(t, client, portal.URL+"/admin/system/backup/settings", settings, 400)
	}
	s.limit = newLimiter()
	settings.Set("backup_time", "07:35")
	postForm(t, client, portal.URL+"/admin/system/backup/settings", settings, 303)
	saved, err := backup.LoadConfig(state)
	if err != nil || saved != c {
		t.Fatalf("settings changed schedule or installation: %+v %v", saved, err)
	}
	response, err := client.PostForm(portal.URL+"/admin/system/backup/key", base)
	if err != nil {
		t.Fatal(err)
	}
	keyBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(keyBody) != hex.EncodeToString(key)+"\n" || response.Header.Get("Cache-Control") != "no-store" || !strings.Contains(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("key download failed or lacked private download headers")
	}
	keyAfter, _ := backup.ReadKey(filepath.Join(state, "recovery.key"))
	if !bytes.Equal(key, keyAfter) {
		t.Fatal("download rotated recovery key")
	}
	if err = os.Remove(filepath.Join(state, "recovery.key")); err != nil {
		t.Fatal(err)
	}
	postForm(t, client, portal.URL+"/admin/system/backup/key", base, 400)
	if _, err = os.Stat(filepath.Join(state, "recovery.key")); !os.IsNotExist(err) {
		t.Fatal("lost configured key was silently replaced")
	}
	if err = os.WriteFile(filepath.Join(state, "recovery.key"), []byte(hex.EncodeToString(key)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.limit = newLimiter()
	if !strings.Contains(page, "宿主机备份任务未连接") {
		t.Fatal("page pretended runner works")
	}
	postForm(t, client, portal.URL+"/admin/system/backup/run", url.Values{"csrf_token": {"bad"}, "admin_password": {password}}, 403)
	postForm(t, client, portal.URL+"/admin/system/backup/run", url.Values{"csrf_token": {csrf}, "admin_password": {"wrong"}}, 403)
	postForm(t, client, portal.URL+"/admin/system/backup/run", base, 303)
	postForm(t, client, portal.URL+"/admin/system/backup/run", base, 409)
	postForm(t, client, portal.URL+"/admin/system/backup/settings", settings, 409)
	postForm(t, client, portal.URL+"/admin/system/backup/key", base, 409)
	status, _ := backup.LoadStatus(state)
	if !backup.Pending(state) || !status.LastSuccessAt.IsZero() {
		t.Fatal("queued backup falsely reported success")
	}
	if err = os.Remove(filepath.Join(state, "pending")); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err = backup.Encrypt(&encrypted, strings.NewReader("encrypted upload test"), key); err != nil {
		t.Fatal(err)
	}
	upload := func(fields url.Values, archive []byte, extra bool, want int) {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for _, name := range []string{"csrf_token", "admin_password", "restore_mode", "confirm", "recovery_key"} {
			if value := fields.Get(name); value != "" {
				_ = writer.WriteField(name, value)
			}
		}
		file, _ := writer.CreateFormFile("backup_file", "../../portal.db")
		_, _ = file.Write(archive)
		if extra {
			_ = writer.WriteField("destination", "/root/cliproxy-portal/data")
		}
		_ = writer.Close()
		req, _ := http.NewRequest("POST", portal.URL+"/admin/system/backup/restore", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			b, _ := io.ReadAll(response.Body)
			t.Fatalf("restore=%d want=%d body=%s", response.StatusCode, want, b)
		}
	}
	fields := url.Values{"csrf_token": {csrf}, "admin_password": {password}, "restore_mode": {backup.DatabaseRestoreMode}, "confirm": {"replace-current-data"}}
	fields.Set("admin_password", "wrong")
	upload(fields, encrypted.Bytes(), false, 403)
	fields.Set("admin_password", password)
	fields.Set("csrf_token", "wrong")
	upload(fields, encrypted.Bytes(), false, 403)
	fields.Set("csrf_token", csrf)
	// Reset only this fixture's counter to exercise extra validation cases.
	s.limit = newLimiter()
	fields.Set("confirm", "1")
	upload(fields, encrypted.Bytes(), false, 400)
	fields.Del("confirm")
	upload(fields, encrypted.Bytes(), false, 400)
	fields.Set("confirm", "replace-current-data")
	upload(fields, []byte("plaintext secret"), false, 400)
	upload(fields, encrypted.Bytes(), true, 400)
	if backup.RestorePending(state) {
		t.Fatal("invalid restore request published")
	}
	upload(fields, encrypted.Bytes(), false, 303)
	upload(fields, encrypted.Bytes(), false, 409)
	postForm(t, client, portal.URL+"/admin/system/backup/run", base, 409)
	restored, _ := backup.LoadRestoreStatus(state)
	if !backup.RestorePending(state) || !restored.FinishedAt.IsZero() {
		t.Fatal("restore incorrectly reported success")
	}
	logs, _ := st.ListAudit(t.Context(), 100, 0)
	settingsAudited, keyAudited := false, false
	for _, row := range logs {
		settingsAudited = settingsAudited || row.Action == "backup.settings.update"
		keyAudited = keyAudited || row.Action == "backup.key.download"
		if strings.Contains(row.Detail, "test-only-github-secret-token") || strings.Contains(row.Detail, hex.EncodeToString(key)) {
			t.Fatal("secret in audit")
		}
	}
	if !settingsAudited || !keyAudited {
		t.Fatal("settings or key download not audited")
	}
	if err = st.SetUserRole(t.Context(), admin.ID, domain.RoleUser); err != nil {
		t.Fatal(err)
	}
	postForm(t, client, portal.URL+"/admin/system/backup/run", base, 403)
	postForm(t, client, portal.URL+"/admin/system/backup/settings", settings, 403)
	postForm(t, client, portal.URL+"/admin/system/backup/key", base, 403)
	upload(fields, encrypted.Bytes(), false, 403)
	postForm(t, &http.Client{CheckRedirect: client.CheckRedirect}, portal.URL+"/admin/system/backup/run", base, 303)
}
