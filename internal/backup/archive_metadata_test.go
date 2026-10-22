package backup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Small database-only fixtures still carry all current-format metadata. Full
// deployment fixtures provide their own approved policy and source snapshots.
func archiveSources(t *testing.T, sources []Source) []Source {
	t.Helper()
	dir := t.TempDir()
	values := []struct {
		name  string
		value any
	}{
		{backupSettingsName, portableBackupSettings{Version: 1, Repository: "owner/private", Hour: 3, Weekday: 1, Retain: 3}},
		{credentialsSnapshotName, credentialSnapshot{Version: 1, Repository: "owner/private", GitHubToken: "test-only-github-secret-token"}},
		{sourcesSnapshotName, append([]Source{}, sources...)},
		{restorePolicySnapshotName, PortalRestorePolicy{Container: "test-portal"}},
	}
	result := append([]Source{}, sources...)
	for _, v := range values {
		found := false
		for _, source := range sources {
			found = found || source.Name == v.name
		}
		if !found {
			file := filepath.Join(dir, filepath.Base(v.name))
			if err := writeJSON(file, v.value); err != nil {
				t.Fatal(err)
			}
			result = append(result, Source{Path: file, Name: v.name, Required: true})
		}
	}
	return result
}

func credentialDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, filepath.FromSlash(credentialsSnapshotName)), credentialSnapshot{Version: 1, Repository: "owner/private", GitHubToken: "test-only-github-secret-token"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runnerSources(t *testing.T, file string, sources []Source) error {
	t.Helper()
	if err := os.Chmod(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	policyFile := filepath.Join(filepath.Dir(file), "restore.json")
	if err := writeJSON(policyFile, PortalRestorePolicy{Database: sources[0].Path, Container: "test-portal", HealthURL: "http://127.0.0.1:18080/healthz"}); err != nil {
		return err
	}
	if err := os.Chmod(policyFile, 0o600); err != nil {
		return err
	}
	// A Linux runner can discover installed collector units. Keep their source
	// files inside the temporary fixture rather than depending on host scripts.
	ops := filepath.Join(filepath.Dir(filepath.Dir(sources[0].Path)), "ops")
	if err := os.MkdirAll(ops, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"collect-host-log-metrics.py", "collect-esxi-metrics.py"} {
		if err := os.WriteFile(filepath.Join(ops, name), []byte("# Test collector reference; never executed.\n"), 0o600); err != nil {
			return err
		}
	}
	return writeJSON(file, sources)
}

func TestCurrentFormatRejectsMissingMetadataAndPreviousEncryptionHeader(t *testing.T) {
	sources, _ := fixture(t)
	for _, name := range []string{backupSettingsName, credentialsSnapshotName, sourcesSnapshotName, restorePolicySnapshotName} {
		t.Run(name, func(t *testing.T) {
			all := archiveSources(t, sources)
			for i, source := range all {
				if source.Name == name {
					all = append(all[:i], all[i+1:]...)
					break
				}
			}
			if _, _, err := Create(t.Context(), all, t.TempDir(), bytes.Repeat([]byte{4}, 32)); err == nil {
				t.Fatal("incomplete current-format backup accepted")
			}
		})
	}
	var encrypted bytes.Buffer
	key := bytes.Repeat([]byte{4}, 32)
	if err := Encrypt(&encrypted, bytes.NewReader([]byte("test")), key); err != nil {
		t.Fatal(err)
	}
	old := append([]byte{}, encrypted.Bytes()...)
	copy(old, "CPBACK01")
	if err := Decrypt(io.Discard, bytes.NewReader(old), key); err == nil {
		t.Fatal("previous encryption header accepted")
	}
	if _, err := readCredentials(t.TempDir()); err == nil {
		t.Fatal("missing credential snapshot accepted")
	}
}
