package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeGitHub struct {
	private    bool
	corrupt    bool
	denyUpload bool
	created    int
	deleted    []string
	rows       []release
	archive    []byte
	active     release
	title      string
	server     *httptest.Server
}

func fakeClient(t *testing.T) (*fakeGitHub, *GitHub) {
	t.Helper()
	f := &fakeGitHub{private: true}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only-secret-token" {
			t.Error("missing token")
		}
		p := r.URL.Path
		switch {
		case p == "/repos/owner/private":
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "owner/private", "private": f.private})
		case p == "/repos/owner/private/releases" && r.Method == http.MethodPost:
			var payload struct {
				Name  string `json:"name"`
				Tag   string `json:"tag_name"`
				Body  string `json:"body"`
				Draft bool   `json:"draft"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if !payload.Draft {
				t.Error("backup must upload as a draft first")
			}
			f.created++
			f.title = payload.Name
			f.active = release{ID: 999, Tag: payload.Tag, Body: payload.Body, Draft: true, URL: "https://github.com/owner/private/releases/tag/" + payload.Tag}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(f.active)
		case p == "/repos/owner/private/releases/999/assets" && r.Method == http.MethodPost:
			if f.denyUpload {
				http.Error(w, "private-secret-error-details", 403)
				return
			}
			body, _ := io.ReadAll(r.Body)
			name := r.URL.Query().Get("name")
			if name == archiveName {
				f.archive = body
			}
			a := asset{ID: 111, Name: name, State: "uploaded", Size: int64(len(body))}
			f.active.Assets = append(f.active.Assets, a)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(a)
		case p == "/repos/owner/private/releases/assets/111":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("download must request binary content")
			}
			if f.corrupt {
				_, _ = w.Write([]byte("corrupted"))
			} else {
				_, _ = w.Write(f.archive)
			}
		case p == "/repos/owner/private/releases/999" && r.Method == http.MethodPatch:
			f.active.Draft = false
			f.rows = append(f.rows, f.active)
			_ = json.NewEncoder(w).Encode(f.active)
		case p == "/repos/owner/private/releases" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.rows)
		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, p)
			w.WriteHeader(204)
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(f.server.Close)
	g := NewGitHub("test-only-secret-token")
	g.api = f.server.URL
	g.uploads = f.server.URL
	g.client = f.server.Client()
	g.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return f, g
}

func TestGitHubPrivateUploadDownloadVerificationAndScopedRetention(t *testing.T) {
	f, g := fakeClient(t)
	c := Config{Repository: "owner/private", Instance: "0123456789abcdef", Retain: 2}
	for i := 1; i <= 3; i++ {
		tag := fmt.Sprintf("%s%s-2026100%dT030000Z-abcdef12", releaseTagPrefix, c.Instance, i)
		f.rows = append(f.rows, release{ID: int64(i), Tag: tag, Body: releaseMarker + "\nInstance: " + c.Instance + "\n", Assets: []asset{{Name: archiveName, State: "uploaded", Size: 10}}})
	}
	f.rows = append(f.rows, release{ID: 20, Tag: "v1.0", Body: "software release"}, release{ID: 21, Tag: releaseTagPrefix + "1111111111111111-20261001T030000Z-abcdef12", Body: releaseMarker + "\nInstance: 1111111111111111\n"}, release{ID: 22, Tag: releaseTagPrefix + c.Instance + "-20260901T030000Z-abcdef12", Body: releaseMarker + "\nInstance: " + c.Instance + "\n", Draft: true})
	file := filepath.Join(t.TempDir(), "backup")
	_ = os.WriteFile(file, []byte("already-encrypted-file"), 0o600)
	tag := releaseTagPrefix + c.Instance + "-20261005T030000Z-123456ab"
	link, size, sha, err := g.Upload(t.Context(), c, file, tag)
	if err != nil || size != 22 || sha == "" || !strings.HasSuffix(link, tag) {
		t.Fatalf("upload link=%s size=%d err=%v", link, size, err)
	}
	if len(f.active.Assets) != 1 || f.active.Assets[0].Name != archiveName {
		t.Fatal("backup must not upload a restore script")
	}
	if !strings.HasPrefix(f.title, "门户备份 ") || !strings.HasPrefix(f.active.Body, releaseMarker+"\n") || strings.Contains(f.title+f.active.Body, "lightweight") || strings.Contains(f.title, "轻量") {
		t.Fatal("new release must use updated title and description")
	}
	if err = g.Retain(t.Context(), c, tag); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 4 {
		t.Fatalf("deleted=%v", f.deleted)
	}
	for _, p := range f.deleted {
		if strings.Contains(p, "/20") || strings.Contains(p, "/21") || strings.Contains(p, "/22") {
			t.Fatal("deleted unrelated release")
		}
	}
	if !strings.HasSuffix(f.deleted[0], "/2") || !strings.HasSuffix(f.deleted[2], "/1") {
		t.Fatal("retention did not delete oldest own backups")
	}
}

func TestRetentionOrdersMixedNamingByBackupTime(t *testing.T) {
	f, g := fakeClient(t)
	c := Config{Repository: "owner/private", Instance: "0123456789abcdef", Retain: 3}
	for i, prefix := range []string{legacyReleaseTagPrefix, releaseTagPrefix, legacyReleaseTagPrefix, releaseTagPrefix} {
		marker, name := releaseMarker, archiveName
		if prefix == legacyReleaseTagPrefix {
			marker, name = legacyReleaseMarker, legacyArchiveName
		}
		f.rows = append(f.rows, release{ID: int64(i + 1), Tag: fmt.Sprintf("%s%s-2026100%dT030000Z-abcdef12", prefix, c.Instance, i+1), Body: marker + "\nInstance: " + c.Instance + "\n", Assets: []asset{{Name: name, State: "uploaded", Size: 10}}})
	}
	newest := releaseTagPrefix + c.Instance + "-20261005T030000Z-abcdef12"
	f.rows = append(f.rows, release{ID: 5, Tag: newest, Body: releaseMarker + "\nInstance: " + c.Instance + "\n", Assets: []asset{{Name: archiveName, State: "uploaded", Size: 10}}})
	for _, r := range []release{
		{ID: 10, Tag: releaseTagPrefix + c.Instance + "-20260901T030000Z-abcdef12", Body: "software release", Assets: []asset{{Name: archiveName, State: "uploaded", Size: 10}}},
		{ID: 11, Tag: legacyReleaseTagPrefix + c.Instance + "-20260902T030000Z-abcdef12", Body: legacyReleaseMarker + "\nInstance: other\n", Assets: []asset{{Name: legacyArchiveName, State: "uploaded", Size: 10}}},
		{ID: 12, Tag: releaseTagPrefix + c.Instance + "-20260903T030000Z-abcdef12", Body: releaseMarker + "\nInstance: " + c.Instance + "\n", Assets: []asset{{Name: archiveName, State: "new", Size: 10}}},
	} {
		f.rows = append(f.rows, r)
	}
	if err := g.Retain(t.Context(), c, newest); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 4 || !strings.HasSuffix(f.deleted[0], "/2") || !strings.HasSuffix(f.deleted[2], "/1") {
		t.Fatalf("mixed prefixes must retain dates 5, 4, 3, not sort by prefix: %v", f.deleted)
	}
	f.deleted = nil
	if err := g.Retain(t.Context(), c, releaseTagPrefix+c.Instance+"-20261006T030000Z-abcdef12"); err == nil || len(f.deleted) != 0 {
		t.Fatal("missing newest backup must not delete anything")
	}
}

func TestPublicRepositoryCorruptDownloadsAndUploadErrorsFailClosed(t *testing.T) {
	for _, mode := range []string{"public", "corrupt", "denied"} {
		t.Run(mode, func(t *testing.T) {
			f, g := fakeClient(t)
			f.private = mode != "public"
			f.corrupt = mode == "corrupt"
			f.denyUpload = mode == "denied"
			file := filepath.Join(t.TempDir(), "backup")
			_ = os.WriteFile(file, []byte("encrypted-data"), 0o600)
			_, _, _, err := g.Upload(t.Context(), Config{Repository: "owner/private", Instance: "0123456789abcdef"}, file, "tag")
			if err == nil || strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), g.token) {
				t.Fatalf("unsafe error %v", err)
			}
			if mode == "public" && f.created != 0 {
				t.Fatal("wrote public repository")
			}
			if len(f.deleted) != 0 || !f.active.Draft && mode != "public" {
				t.Fatal("unverified upload published or deleted old backups")
			}
		})
	}
}

func TestRunnerManualAndWeeklyJobsPersistSuccessAndFailure(t *testing.T) {
	sources, _ := fixture(t)
	dir := t.TempDir()
	key, err := EnsureKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = key
	instance, _ := NewInstance()
	c := Config{Enabled: true, Repository: "owner/private", Hour: 3, Weekday: 1, Retain: 3, KeySaved: true, Instance: instance}
	if err = SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err = SaveToken(dir, "test-only-secret-token"); err != nil {
		t.Fatal(err)
	}
	sourcesFile := filepath.Join(t.TempDir(), "sources.json")
	if err = writeJSON(sourcesFile, sources); err != nil {
		t.Fatal(err)
	}
	f, g := fakeClient(t)
	newClient := func(string) *GitHub { return g }
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) // 08:00 Shanghai
	if err = tick(context.Background(), dir, sourcesFile, now, newClient); err != nil {
		t.Fatal(err)
	}
	status, err := LoadStatus(dir)
	if err != nil || status.Running || status.LastSuccessAt.IsZero() || status.Size == 0 || status.LastAttemptDay != "2026-10-05" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if status.Phase != "" || status.TotalDurationMS <= 0 || len(status.StageDurationMS) != 8 {
		t.Fatalf("missing stage metrics: %+v", status)
	}
	for _, stage := range []string{"preflight", "snapshot", "compress_encrypt", "local_verify", "upload", "download_verify", "publish", "retention"} {
		if _, ok := status.StageDurationMS[stage]; !ok {
			t.Fatalf("missing stage %s", stage)
		}
	}
	if err = tick(t.Context(), dir, sourcesFile, now.Add(time.Hour), newClient); err != nil || f.created != 1 {
		t.Fatal("same day retried scheduled backup")
	}
	_ = Request(dir)
	f.corrupt = true
	if err = tick(t.Context(), dir, sourcesFile, now.Add(2*time.Hour), newClient); err == nil {
		t.Fatal("corrupt remote backup accepted")
	}
	failed, _ := LoadStatus(dir)
	if failed.Running || !failed.LastSuccessAt.Equal(status.LastSuccessAt) || failed.ReleaseURL != status.ReleaseURL || !strings.Contains(failed.Message, "失败") || Pending(dir) {
		t.Fatal("failure lost successful history or left running state")
	}
	if failed.Phase != "download_verify" || len(failed.StageDurationMS) != 6 {
		t.Fatalf("failed-stage metrics or reset incorrect: %+v", failed)
	}
	if _, err = os.Stat(filepath.Join(dir, "runner.lock")); !os.IsNotExist(err) {
		t.Fatal("runner lock not cleaned")
	}
	children, _ := os.ReadDir(dir)
	for _, child := range children {
		if strings.HasPrefix(child.Name(), ".job-") {
			t.Fatal("plaintext job directory not cleaned")
		}
	}
	if len(f.deleted) != 0 {
		t.Fatal("failure pruned old backups")
	}
	if RunnerSeen(dir).IsZero() {
		t.Fatal("runner heartbeat missing")
	}
}

func TestNoCredentialRedirectLeaks(t *testing.T) {
	g := NewGitHub("test-only-secret-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example/steal", 302)
	}))
	defer server.Close()
	g.api = server.URL
	if err := g.verifyAsset(t.Context(), "owner/private", 1, Entry{Size: 1, SHA256: strconv.Itoa(1)}); err == nil {
		t.Fatal("unsafe download redirect accepted")
	}
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, strings.NewReader("private"), bytes.Repeat([]byte{5}, 32)); err != nil {
		t.Fatal(err)
	}
}
