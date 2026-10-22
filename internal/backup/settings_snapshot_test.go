package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerArchiveIncludesPortableBackupSettingsWithoutCredentials(t *testing.T) {
	sources, _ := fixture(t)
	dir := t.TempDir()
	if _, err := EnsureKey(dir); err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true, Repository: "owner/private", Hour: 4, Minute: 35, Weekday: 2, Retain: 7, KeySaved: true, Instance: "0123456789abcdef"}
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := SaveToken(dir, "test-only-secret-token"); err != nil {
		t.Fatal(err)
	}
	sourcesFile := filepath.Join(t.TempDir(), "sources.json")
	if err := writeJSON(sourcesFile, sources); err != nil {
		t.Fatal(err)
	}
	if err := Request(dir); err != nil {
		t.Fatal(err)
	}
	f, g := fakeClient(t)
	if err := tick(t.Context(), dir, sourcesFile, time.Now(), func(string) *GitHub { return g }); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.cbackup")
	if err := os.WriteFile(archive, f.archive, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "restored")
	if err := Restore(t.Context(), archive, filepath.Join(dir, "recovery.key"), output); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(output, filepath.FromSlash(backupSettingsName))
	v, err := readBackupSettings(file)
	if err != nil || !v.Enabled || v.Repository != c.Repository || v.Hour != 4 || v.Minute != 35 || v.Weekday != 2 || v.Retain != 7 {
		t.Fatal("backup preferences did not survive encrypted round trip", v, err)
	}
	raw, _ := os.ReadFile(file)
	for _, forbidden := range []string{"token", "recovery", "instance", "key_saved", c.Instance} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("nonportable credential or identity included", forbidden)
		}
	}
	var savedSources []Source
	if err := readJSON(filepath.Join(output, filepath.FromSlash(sourcesSnapshotName)), &savedSources); err != nil || len(savedSources) != len(sources) {
		t.Fatal("runner did not include configured sources automatically", savedSources, err)
	}
}

func archivedSettings(t *testing.T, restored string) {
	t.Helper()
	file := filepath.Join(restored, filepath.FromSlash(backupSettingsName))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(file, portableBackupSettings{Version: 1, Enabled: true, Repository: "owner/private", Hour: 4, Minute: 35, Weekday: 2, Retain: 7}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupSettingsRestoreAndRollbackAreOneTransaction(t *testing.T) {
	for _, mode := range []string{DatabaseRestoreMode, RebuildRestoreMode} {
		for _, failure := range []string{"none", "health", "interrupted"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				state, restored, p := restoreFixture(t)
				original := Config{Enabled: false, Repository: "owner/private", Hour: 3, Weekday: 1, Retain: 3, KeySaved: true, Instance: "fedcba9876543210"}
				if err := SaveConfig(state, original); err != nil {
					t.Fatal(err)
				}
				key, err := EnsureKey(state)
				if err != nil {
					t.Fatal(err)
				}
				if err = SaveToken(state, "current-secret-token"); err != nil {
					t.Fatal(err)
				}
				archivedSettings(t, restored)
				svc := &restoreService{healthFail: failure == "health", alwaysFail: failure == "interrupted"}
				_, err = replacePortalDatabase(t.Context(), state, restored, p, svc, mode)
				if failure == "none" && err != nil || failure != "none" && err == nil {
					t.Fatal("incorrect transaction result", err)
				}
				if failure == "interrupted" {
					svc.alwaysFail = false
					if err = recoverDatabaseTransaction(t.Context(), state, p, svc); err != nil {
						t.Fatal(err)
					}
				}
				got, err := LoadConfig(state)
				if err != nil {
					t.Fatal(err)
				}
				if failure == "none" {
					if !got.Enabled || got.Hour != 4 || got.Minute != 35 || got.Weekday != 2 || got.Retain != 7 || got.Instance != original.Instance || !got.KeySaved {
						t.Fatal("preferences not restored or current identity lost", got)
					}
					checkRestoreDB(t, p.Database, "backup", true)
				} else {
					if got != original {
						t.Fatal("failed restore did not roll back backup settings", got)
					}
					checkRestoreDB(t, p.Database, "live", false)
				}
				after, _ := ReadKey(filepath.Join(state, "recovery.key"))
				token, _ := LoadToken(state)
				if !bytes.Equal(after, key) || token != "current-secret-token" {
					t.Fatal("restore replaced current credentials")
				}
			})
		}
	}
}

func TestBackupSettingsFreshRestoreAndOldArchives(t *testing.T) {
	for _, included := range []bool{true, false} {
		state, restored, p := restoreFixture(t)
		if included {
			archivedSettings(t, restored)
		}
		if _, err := replacePortalDatabase(t.Context(), state, restored, p, &restoreService{}); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(state)
		if err != nil || c.Enabled || c.KeySaved {
			t.Fatal("fresh installation scheduled backup without credentials", c, err)
		}
		if included && (c.Repository != "owner/private" || c.Hour != 4 || c.Minute != 35 || c.Retain != 7 || !instancePattern.MatchString(c.Instance)) {
			t.Fatal("fresh restore lost portable settings", c)
		}
		if !included && c.Repository != "" {
			t.Fatal("old archive invented backup settings", c)
		}
	}
}

func TestInvalidBackupSettingsRejectedBeforeStoppingServices(t *testing.T) {
	state, restored, p := restoreFixture(t)
	archivedSettings(t, restored)
	file := filepath.Join(restored, filepath.FromSlash(backupSettingsName))
	raw, _ := os.ReadFile(file)
	raw = []byte(strings.Replace(string(raw), `"version": 1`, `"version": 1, "github_token": "must-not-import"`, 1))
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	svc := &restoreService{}
	if _, err := replacePortalDatabase(t.Context(), state, restored, p, svc); err == nil || svc.stops != 0 {
		t.Fatal("unapproved backup setting accepted or services stopped", err)
	}
}
