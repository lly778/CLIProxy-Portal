package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueuedRestoreDoesNotTouchLiveDataOrRequireGitHub(t *testing.T) {
	sources, dir := fixture(t)
	work := filepath.Join(dir, "archive")
	_ = os.Mkdir(work, 0o700)
	key := bytes.Repeat([]byte{7}, 32)
	archive, _, err := Create(t.Context(), sources, work, key)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(archive)
	state := filepath.Join(dir, "state")
	if err = RequestRestore(state, bytes.NewReader(content), key, DatabaseRestoreMode); err != nil {
		t.Fatal(err)
	}
	if err = RequestRestore(state, bytes.NewReader(content), key, DatabaseRestoreMode); err == nil {
		t.Fatal("duplicate restore allowed")
	}
	if err = tick(t.Context(), state, "missing-config", time.Now(), func(string) *GitHub { t.Fatal("restore accessed GitHub"); return nil }); err == nil {
		t.Fatal("unconfigured restore policy accepted")
	}
	s, err := LoadRestoreStatus(state)
	if err != nil || s.Running || s.FinishedAt.IsZero() || s.Rollback != "" || !strings.HasPrefix(s.Message, "恢复失败") {
		t.Fatalf("restore status: %+v %v", s, err)
	}
	if RestorePending(state) {
		t.Fatal("restore upload retained")
	}
	if _, err = os.Stat(filepath.Join(state, "runner.lock")); !os.IsNotExist(err) {
		t.Fatal("runner lock retained")
	}
	original, _ := os.ReadFile(filepath.Join(dir, "app.key"))
	if string(original) != "original-application-secret" {
		t.Fatal("live secret changed")
	}
	if err = RequestRestore(state, bytes.NewReader(content), bytes.Repeat([]byte{3}, 32), DatabaseRestoreMode); err != nil {
		t.Fatal(err)
	}
	if err = Tick(t.Context(), state, "missing-config"); err == nil {
		t.Fatal("wrong key accepted")
	}
	s, _ = LoadRestoreStatus(state)
	if s.Running || s.Rollback != "" || !strings.HasPrefix(s.Message, "恢复失败") {
		t.Fatalf("failure status: %+v", s)
	}
	entries, _ := filepath.Glob(filepath.Join(state, ".restore-job-*"))
	if len(entries) != 0 {
		t.Fatal("failed restore left plaintext directory")
	}
	if err = RequestRestore(state, strings.NewReader("plain text"), key, DatabaseRestoreMode); err == nil {
		t.Fatal("plaintext accepted")
	}
}

func TestHostConfigureAndKeyExportNeverPrintOrOverwriteKeys(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	export := filepath.Join(dir, "export.key")
	if err := ExportKey(state, export); err != nil {
		t.Fatal(err)
	}
	key, _ := ReadKey(export)
	if err := ExportKey(state, export); err == nil {
		t.Fatal("existing export overwritten")
	}
	if err := ExportKey(state, filepath.Join(state, "copy.key")); err == nil {
		t.Fatal("key exported into backup state")
	}
	c := Config{Repository: "owner/private", Hour: 3, Retain: 3, Enabled: true, KeySaved: true}
	tokenFile := filepath.Join(dir, "token")
	_ = os.WriteFile(tokenFile, []byte("test-only-secret-token\n"), 0o600)
	f, g := fakeClient(t)
	if err := configure(t.Context(), state, c, tokenFile, func(string) *GitHub { return g }); err != nil {
		t.Fatal(err)
	}
	saved, _ := LoadConfig(state)
	if saved.Instance == "" || !saved.Enabled {
		t.Fatal("host configuration not saved")
	}
	c.Instance = saved.Instance
	f.private = false
	if err := configure(t.Context(), state, c, tokenFile, func(string) *GitHub { return g }); err == nil {
		t.Fatal("public repo accepted")
	}
	c.Enabled = false
	if err := configure(t.Context(), state, c, "", func(string) *GitHub { t.Fatal("offline pause accessed GitHub"); return nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := ReadKey(filepath.Join(state, "recovery.key"))
	if !bytes.Equal(key, after) {
		t.Fatal("configuration changed recovery key")
	}
	configBytes, _ := os.ReadFile(filepath.Join(state, "config.json"))
	if bytes.Contains(configBytes, []byte("secret-token")) {
		t.Fatal("token in config")
	}
}
