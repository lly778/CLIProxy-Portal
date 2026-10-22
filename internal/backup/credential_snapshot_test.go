package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEncryptedBackupIncludesCredentialsButNotRecoveryKey(t *testing.T) {
	sources, _ := fixture(t)
	state, work, etc := t.TempDir(), t.TempDir(), t.TempDir()
	if err := SaveToken(state, "test-only-github-secret-token"); err != nil {
		t.Fatal(err)
	}
	esxi := esxiCredentials{URL: "https://192.0.2.3", Username: "test-root", Password: "secret-for-test", Fingerprint: strings.Repeat("a", 64)}
	if err := writeJSON(filepath.Join(etc, "cliproxy-esxi/esxi.json"), esxi); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(etc, "systemd/system/cloudflared-cliproxy.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("test-only-tunnel-", 5)
	if err := os.WriteFile(unit, []byte("ExecStart=/bin/cloudflared tunnel run --token "+token), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := snapshotCredentials(state, work, "owner/private", etc)
	if err != nil {
		t.Fatal(err)
	}
	if source.Name != credentialsSnapshotName {
		t.Fatal("wrong archive name")
	}
	key := bytes.Repeat([]byte{5}, 32)
	keyFile := filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, manifest, err := Create(t.Context(), archiveSources(t, append(sources, source)), work, key)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "restored")
	if err := Restore(t.Context(), archive, keyFile, output); err != nil {
		t.Fatal(err)
	}
	v, err := readCredentials(output)
	if err != nil || v == nil || v.GitHubToken != "test-only-github-secret-token" || v.CloudflareToken != token || !reflect.DeepEqual(v.ESXi, &esxi) {
		t.Fatal("credentials failed round trip; values withheld")
	}
	for _, entry := range manifest.Entries {
		if strings.Contains(entry.Name, "recovery.key") {
			t.Fatal("recovery key archived")
		}
	}
	raw, _ := os.ReadFile(archive)
	for _, secret := range []string{token, esxi.Password, v.GitHubToken} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("plaintext credential in encrypted archive")
		}
	}
}

type credentialTestService struct{ restoreService }

func (s *credentialTestService) prepareCredentials(restored, dir string, p PortalRestorePolicy) (restoredCredentials, error) {
	for name, value := range map[string]string{"github.token": "restored-github-token-value", "recovery.key": strings.Repeat("05", 32)} {
		if err := atomicWrite(filepath.Join(restored, "portal/backup/runtime", name), []byte(value), 0o600); err != nil {
			return restoredCredentials{}, err
		}
	}
	return restoredCredentials{GitHub: true, Key: true}, nil
}

func TestCredentialsAndScheduleCommitOrRollbackTogether(t *testing.T) {
	for _, failure := range []string{"none", "health", "interrupted"} {
		t.Run(failure, func(t *testing.T) {
			restored, p, _ := deploymentFixture(t)
			state := filepath.Join(filepath.Dir(filepath.Dir(p.Database)), "state")
			archivedSettings(t, restored)
			old := Config{Repository: "old/private", Instance: "1111111111111111", KeySaved: true}
			if err := writeJSON(filepath.Join(state, "config.json"), old); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"github.token": "old-github-token-value", "recovery.key": strings.Repeat("04", 32)} {
				if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			svc := &credentialTestService{restoreService: restoreService{healthFail: failure == "health", alwaysFail: failure == "interrupted"}}
			_, err := replacePortalDatabase(context.Background(), state, restored, p, svc, RebuildRestoreMode)
			if (err == nil) != (failure == "none") {
				t.Fatalf("unexpected result %v", err)
			}
			if failure == "interrupted" {
				svc.alwaysFail = false
				if !DatabaseRecoveryPending(state) {
					t.Fatal("missing recovery transaction")
				}
				if err := recoverDatabaseTransaction(t.Context(), state, p, svc); err != nil {
					t.Fatal(err)
				}
			}
			var got Config
			_ = readJSON(filepath.Join(state, "config.json"), &got)
			token, _ := os.ReadFile(filepath.Join(state, "github.token"))
			key, _ := os.ReadFile(filepath.Join(state, "recovery.key"))
			if failure == "none" {
				if !got.Enabled || got.Instance != old.Instance || string(token) != "restored-github-token-value" || string(key) != strings.Repeat("05", 32) {
					t.Fatal("credentials/settings did not commit together")
				}
			} else {
				if got.Repository != old.Repository || string(token) != "old-github-token-value" || string(key) != strings.Repeat("04", 32) {
					t.Fatal("rollback failed to restore credentials/settings")
				}
			}
		})
	}
}

func TestInvalidCredentialErrorsNeverExposeSecret(t *testing.T) {
	for _, v := range []credentialSnapshot{
		{Version: 1, Repository: "owner/private", GitHubToken: "bad-secret"},
		{Version: 1, Repository: "owner/private", GitHubToken: strings.Repeat("x", 30), CloudflareToken: "bad-secret"},
		{Version: 1, Repository: "owner/private", GitHubToken: strings.Repeat("x", 30), ESXi: &esxiCredentials{URL: "http://bad-secret", Password: "bad-secret"}},
	} {
		_, err := writeCredentialSnapshot(v, t.TempDir())
		if err == nil || strings.Contains(err.Error(), "bad-secret") {
			t.Fatal("unsafe credential validation error")
		}
	}
}

func TestCredentialRestoreSettingsKeepsNewInstallationIdentity(t *testing.T) {
	state := t.TempDir()
	c := Config{Repository: "old/private", Instance: "1111111111111111", KeySaved: false}
	if err := writeJSON(filepath.Join(state, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := writeJSON(candidate, portableBackupSettings{Version: 1, Enabled: true, Repository: "owner/private", Weekday: 1, Hour: 3, Retain: 3}); err != nil {
		t.Fatal(err)
	}
	if err := prepareBackupSettings(state, candidate, true); err != nil {
		t.Fatal(err)
	}
	var result Config
	if err := readJSON(candidate, &result); err != nil || !result.Enabled || !result.KeySaved || result.Instance != c.Instance {
		t.Fatal("credential-ready settings not restored safely")
	}
}
