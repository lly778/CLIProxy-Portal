package backup

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostTasksAndScriptsSurviveBackupWithSeparateTunnelCredentials(t *testing.T) {
	root, units, work := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ops"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, unit := range portalHostUnits {
		if err := os.WriteFile(filepath.Join(units, unit), []byte("[Service]\nExecStart=/usr/bin/true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, script := range []string{"collect-host-log-metrics.py", "collect-esxi-metrics.py"} {
		if err := os.WriteFile(filepath.Join(root, "ops", script), []byte("# credential files are loaded separately\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dropIn := filepath.Join(units, "cliproxy-portal-backup.service.d")
	if err := os.Mkdir(dropIn, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropIn, "override.conf"), []byte("[Service]\nMemoryMax=512M\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wants := filepath.Join(units, "timers.target.wants")
	if err := os.Mkdir(wants, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wants, "cliproxy-portal-backup.timer"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "must-not-copy-the-live-tunnel-token"
	tunnel := filepath.Join(units, "cloudflared.service")
	if err := os.WriteFile(tunnel, []byte("[Service]\nExecStart=/usr/bin/cloudflared tunnel run --token "+token), 0o600); err != nil {
		t.Fatal(err)
	}
	hostSources, err := collectHostTaskSources(root, units, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(hostSources) != 11 {
		t.Fatalf("missing tasks, scripts, override, template or inventory: %d", len(hostSources))
	}
	for _, source := range hostSources {
		raw, err := os.ReadFile(source.Path)
		if err != nil || bytes.Contains(raw, []byte(token)) {
			t.Fatalf("tunnel credentials leaked: %v", err)
		}
		if filepath.Dir(source.Path) != work {
			t.Fatal("live host file returned as disposable snapshot")
		}
	}
	var inventory hostTaskInventory
	if err := readJSON(filepath.Join(work, "host-tasks.json"), &inventory); err != nil || inventory.Version != 1 || len(inventory.Files) != 10 {
		t.Fatalf("inventory: %+v, %v", inventory, err)
	}
	for _, file := range inventory.Files {
		if file.Archive == "portal/host/systemd/cliproxy-portal-backup.timer" && !file.Enabled {
			t.Fatal("timer enable state was lost")
		}
	}
	sources, _ := fixture(t)
	key, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "export.key")
	if err = os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, _, err := Create(t.Context(), archiveSources(t, append(sources, hostSources...)), work, key)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "review")
	if err = Restore(t.Context(), archive, keyFile, output); err != nil {
		t.Fatal(err)
	}
	for _, source := range hostSources {
		want, _ := os.ReadFile(source.Path)
		got, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(source.Name)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("task did not survive authenticated round trip: %s, %v", source.Name, err)
		}
		if err := os.Remove(source.Path); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := os.ReadFile(tunnel); err != nil || !strings.Contains(string(raw), token) {
		t.Fatal("cleaning snapshots changed live tunnel configuration")
	}
}

func TestHostTaskSnapshotRejectsInlineCredentialsAndUnknownFiles(t *testing.T) {
	for _, content := range []string{
		"[Service]\nEnvironment=GITHUB_TOKEN=private-value\n",
		"[Service]\nEnvironment=\"MY_PASSWORD=private-value\"\n",
		"[Service]\nExecStart=/bin/program --token=private-value\n",
	} {
		units := t.TempDir()
		if err := os.WriteFile(filepath.Join(units, portalHostUnits[0]), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := collectHostTaskSources(t.TempDir(), units, t.TempDir())
		if err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("inline credentials were not rejected safely: %v", err)
		}
	}
	for _, name := range []string{"portal/host/systemd/ssh.service", "portal/host/systemd/cliproxy.slice.d/../secret.conf", "portal/host/ops/password.json"} {
		if allowedHostTaskName(name) {
			t.Fatalf("unrelated host file allowed: %s", name)
		}
	}
}
