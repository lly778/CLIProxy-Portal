package backup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRootConfigurationInheritsAppOwner(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires isolated root fixture")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 10001, 10001); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureKey(dir); err != nil {
		t.Fatal(err)
	}
	if err := SaveToken(dir, "test-only-secret-token"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"recovery.key", "github.token"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != 10001 || st.Gid != 10001 || info.Mode().Perm() != 0o600 {
			t.Fatalf("incorrect owner/mode: %s", name)
		}
	}
}

func TestQueuedRestoreRejectsSymlinkFiles(t *testing.T) {
	dir := t.TempDir()
	request := filepath.Join(dir, "request")
	_ = os.Mkdir(request, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "live.key"), []byte("live-secret"), 0o600)
	_ = os.WriteFile(filepath.Join(request, "backup.cbackup"), []byte("CPBACK01"), 0o600)
	_ = writeJSON(filepath.Join(request, "request.json"), restoreRequest{Version: 2, Mode: DatabaseRestoreMode})
	if err := os.Symlink(filepath.Join(dir, "live.key"), filepath.Join(request, "recovery.key")); err != nil {
		t.Fatal(err)
	}
	if err := validateRestoreRequest(request); err == nil {
		t.Fatal("host accepted symlink source")
	}
}
