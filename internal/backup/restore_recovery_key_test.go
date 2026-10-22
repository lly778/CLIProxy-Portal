package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recoveryKeyTestService struct {
	restoreService
	keyFile string
}

func (s *recoveryKeyTestService) prepareRecoveryKey(restored, dir string) (bool, error) {
	return prepareMissingRecoveryKey(restored, dir, s.keyFile)
}

func TestDatabaseRestoreRetainsMissingRecoveryKeyTransactionally(t *testing.T) {
	for _, failure := range []string{"none", "health", "interrupted"} {
		t.Run(failure, func(t *testing.T) {
			state, restored, policy := restoreFixture(t)
			original := Config{Repository: "old/private", Instance: "1111111111111111", KeySaved: false}
			if err := writeJSON(filepath.Join(state, "config.json"), original); err != nil {
				t.Fatal(err)
			}
			keyFile := filepath.Join(t.TempDir(), "request.key")
			if err := os.WriteFile(keyFile, []byte(strings.Repeat("05", 32)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			svc := &recoveryKeyTestService{
				restoreService: restoreService{healthFail: failure == "health", alwaysFail: failure == "interrupted"},
				keyFile:        keyFile,
			}
			_, err := replacePortalDatabase(t.Context(), state, restored, policy, svc, DatabaseRestoreMode)
			if (err == nil) != (failure == "none") {
				t.Fatal("unexpected restore result", err)
			}
			// The request key and decrypted job can disappear after the initial
			// attempt. Crash recovery must use the persisted transaction alone.
			if err := os.Remove(keyFile); err != nil {
				t.Fatal(err)
			}
			if failure == "interrupted" {
				svc.alwaysFail = false
				if !DatabaseRecoveryPending(state) {
					t.Fatal("interrupted restore lost its transaction")
				}
				if err := recoverDatabaseTransaction(t.Context(), state, policy, svc); err != nil {
					t.Fatal(err)
				}
			}
			config, err := LoadConfig(state)
			if err != nil {
				t.Fatal(err)
			}
			key, keyErr := ReadKey(filepath.Join(state, "recovery.key"))
			if failure == "none" {
				if keyErr != nil || !bytes.Equal(key, bytes.Repeat([]byte{5}, 32)) {
					t.Fatal("successful restore did not retain the decrypting key")
				}
				if config.Enabled || config.KeySaved || config.Repository != "owner/private" || config.Instance != original.Instance {
					t.Fatal("key retention unexpectedly confirmed settings or enabled scheduling")
				}
				checkRestoreDB(t, policy.Database, "backup", true)
			} else {
				if !os.IsNotExist(keyErr) || config != original {
					t.Fatal("failed restore did not roll back the key and settings together")
				}
				checkRestoreDB(t, policy.Database, "live", false)
			}
		})
	}
}

func TestDatabaseRestorePreservesExistingRecoveryKey(t *testing.T) {
	state, restored, policy := restoreFixture(t)
	original, err := EnsureKey(state)
	if err != nil {
		t.Fatal(err)
	}
	// An existing key must be preserved even when the request used another key.
	keyFile := filepath.Join(t.TempDir(), "request.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("06", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &recoveryKeyTestService{keyFile: keyFile}
	if _, err := replacePortalDatabase(t.Context(), state, restored, policy, svc, DatabaseRestoreMode); err != nil {
		t.Fatal(err)
	}
	key, err := ReadKey(filepath.Join(state, "recovery.key"))
	if err != nil || !bytes.Equal(original, key) {
		t.Fatal("database restore replaced the installation's existing key")
	}
}

func TestDatabaseRestoreRejectsInvalidRecoveryKeysBeforeStopping(t *testing.T) {
	for _, invalid := range []string{"existing key", "request key"} {
		t.Run(invalid, func(t *testing.T) {
			state, restored, policy := restoreFixture(t)
			keyFile := filepath.Join(t.TempDir(), "request.key")
			if err := os.WriteFile(keyFile, []byte(strings.Repeat("05", 32)), 0o600); err != nil {
				t.Fatal(err)
			}
			file := keyFile
			if invalid == "existing key" {
				file = filepath.Join(state, "recovery.key")
			}
			if err := os.WriteFile(file, []byte("invalid-key"), 0o600); err != nil {
				t.Fatal(err)
			}
			svc := &recoveryKeyTestService{keyFile: keyFile}
			if _, err := replacePortalDatabase(t.Context(), state, restored, policy, svc, DatabaseRestoreMode); err == nil || svc.stops != 0 {
				t.Fatal("invalid key accepted or services stopped")
			}
		})
	}
}
