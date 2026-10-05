package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func managerTestDB(t *testing.T, file, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE usage_events(value TEXT); INSERT INTO usage_events VALUES(?)", value); err != nil {
		t.Fatal(err)
	}
}

func checkManagerRow(t *testing.T, file, expected string) {
	t.Helper()
	db, err := openSQLite(file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err = db.QueryRow("SELECT value FROM usage_events").Scan(&value); err != nil || value != expected {
		t.Fatalf("manager value=%s err=%v", value, err)
	}
}

func TestManagerSnapshotIncludesCommittedWALAndDataKeyNotArchives(t *testing.T) {
	sources, root := fixture(t)
	dbFile := filepath.Join(root, "usage.sqlite")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE usage_events(value TEXT); INSERT INTO usage_events VALUES('committed-manager-WAL')"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(dbFile + "-wal"); err != nil {
		t.Fatal(err)
	}
	dataKey := filepath.Join(root, "data.key")
	if err = os.WriteFile(dataKey, []byte("saved-cpamp-data-encryption-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	archives := filepath.Join(root, "usage-archives")
	if err = os.Mkdir(archives, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(archives, "not-backed-up.jsonl.gz"), []byte("archive-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources = append(sources, Source{Path: dbFile, Name: managerDatabaseName, SQLite: true, Required: true}, Source{Path: dataKey, Name: managerDataKeyName, Required: true})
	work := filepath.Join(root, "work")
	if err = os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{5}, 32)
	keyFile := filepath.Join(root, "recovery.key")
	if err = os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	paused := false
	file, manifest, err := createArchive(t.Context(), sources, work, key, nil, func(collect func() error) error {
		paused = true
		err := collect()
		paused = false
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if paused {
		t.Fatal("compression must happen after resume")
	}
	for _, entry := range manifest.Entries {
		if strings.Contains(entry.Name, "usage-archives") || strings.Contains(entry.Name, "-wal") || strings.Contains(entry.Name, "manager.lock") {
			t.Fatal("unselected manager file archived")
		}
	}
	for _, path := range []string{"files", "archive.tar.gz"} {
		if _, err := os.Stat(filepath.Join(work, path)); !os.IsNotExist(err) {
			t.Fatal("unneeded snapshot/compressed duplicate retained")
		}
	}
	restored := filepath.Join(root, "restored")
	if err = Restore(t.Context(), file, keyFile, restored); err != nil {
		t.Fatal(err)
	}
	checkManagerRow(t, filepath.Join(restored, filepath.FromSlash(managerDatabaseName)), "committed-manager-WAL")
	got, err := os.ReadFile(filepath.Join(restored, filepath.FromSlash(managerDataKeyName)))
	if err != nil || string(got) != "saved-cpamp-data-encryption-key" {
		t.Fatal("data.key not preserved")
	}
	if err = validateManagerBackup(t.Context(), restored, true); err != nil {
		t.Fatal(err)
	}
	if maxArchiveSize <= 638275584 || MaxEncryptedSize >= 2<<30 {
		t.Fatal("capacity doesn't support the installed manager database or GitHub bound")
	}
	for _, partial := range [][]Source{sources[:len(sources)-1], append(append([]Source{}, sources[:len(sources)-2]...), sources[len(sources)-1]), append(append([]Source{}, sources...), Source{Path: archives, Name: "upstream/cpamp/usage-archives", Directory: true})} {
		if err = validateSources(partial); err == nil {
			t.Fatal("partial/forbidden manager backup accepted")
		}
	}
}

func fakeManager(t *testing.T, p PortalRestorePolicy, failure string) (*dockerPortal, *[]string) {
	t.Helper()
	running := true
	commands := []string{}
	d := &dockerPortal{policy: p}
	d.execute = func(_ context.Context, args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(args, " "))
		switch args[0] {
		case "ps":
			if failure == "shared-volume" {
				return []byte("unapproved-writer"), nil
			}
			return []byte(p.ManagerContainer), nil
		case "stop":
			if args[len(args)-1] != p.ManagerContainer {
				t.Fatal("snapshot stopped portal/CPA")
			}
			if failure == "stop" {
				return nil, errors.New("stop failed")
			}
			running = false
			return nil, nil
		case "start":
			if args[len(args)-1] != p.ManagerContainer {
				t.Fatal("snapshot started portal/CPA")
			}
			if failure == "start" {
				return nil, errors.New("start failed")
			}
			running = true
			return nil, nil
		case "inspect":
			source := filepath.Dir(p.ManagerDatabase)
			if failure == "mount" {
				source += "-wrong"
			}
			return json.Marshal(map[string]any{
				"State":  map[string]any{"Running": running, "Health": map[string]string{"Status": "healthy"}},
				"Mounts": []map[string]any{{"Source": source, "Destination": "/data", "RW": true, "Name": p.ManagerVolume, "Type": "volume"}},
				"Env":    []string{"USAGE_DB_PATH=/data/usage.sqlite", "CPA_MANAGER_DATA_KEY_PATH=/data/data.key"},
				"Labels": map[string]string{"com.docker.compose.project": p.ProjectName, "com.docker.compose.service": p.ManagerService},
			})
		}
		t.Fatalf("unexpected manager operation: %v", args)
		return nil, nil
	}
	return d, &commands
}

func TestManagerPauseResumesOnSnapshotErrorAndCancellation(t *testing.T) {
	for _, scenario := range []string{"valid", "collect", "cancel", "stop", "start", "mount", "shared-volume"} {
		t.Run(scenario, func(t *testing.T) {
			state, _, p := restoreFixture(t)
			d, commands := fakeManager(t, p, scenario)
			collected := false
			err := d.withManagerSnapshot(t.Context(), state, func() error {
				collected = true
				if len(*commands) == 0 || !strings.Contains(strings.Join(*commands, "\n"), "stop --time 30 "+p.ManagerContainer) {
					t.Fatal("snapshot before pause")
				}
				if !ManagerBackupPending(state) {
					t.Fatal("no durable pause journal")
				}
				if scenario == "collect" {
					return errors.New("snapshot failed")
				}
				if scenario == "cancel" {
					return context.Canceled
				}
				return nil
			})
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("err=%v", err)
			}
			if scenario == "mount" || scenario == "shared-volume" {
				if collected || strings.Contains(strings.Join(*commands, "\n"), "stop ") {
					t.Fatal("unsafe volume paused")
				}
			} else if !strings.Contains(strings.Join(*commands, "\n"), "start "+p.ManagerContainer) {
				t.Fatal("manager not restarted on failure")
			}
			if ManagerBackupPending(state) != (scenario == "start") {
				t.Fatal("pause journal cleanup/retention incorrect")
			}
		})
	}
}

func TestInterruptedManagerSnapshotRecoveryNeverTouchesPortal(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-container", "start-failure"} {
		t.Run(scenario, func(t *testing.T) {
			state, _, p := restoreFixture(t)
			marker := filepath.Join(state, managerPauseName)
			if err := os.Mkdir(marker, 0o700); err != nil {
				t.Fatal(err)
			}
			pause := managerPause{Version: 1, Container: p.ManagerContainer}
			if scenario == "wrong-container" {
				pause.Container = p.Container
			}
			if err := writeJSON(filepath.Join(marker, "state.json"), pause); err != nil {
				t.Fatal(err)
			}
			policyDir := t.TempDir()
			if err := os.Chmod(policyDir, 0o700); err != nil {
				t.Fatal(err)
			}
			policyFile := filepath.Join(policyDir, "restore.json")
			if err := writeJSON(policyFile, p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "runner.lock"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			failure := ""
			if scenario == "start-failure" {
				failure = "start"
			}
			d, commands := fakeManager(t, p, failure)
			err := recoverManagerSnapshotWith(t.Context(), state, policyFile, func(PortalRestorePolicy) *dockerPortal { return d })
			if err == nil {
				t.Fatal("interrupted backup must not report success")
			}
			if ManagerBackupPending(state) != (scenario != "valid") {
				t.Fatal("interruption journal cleanup mismatch")
			}
			if scenario == "wrong-container" && len(*commands) > 0 {
				t.Fatal("unapproved interruption targeted a service")
			}
			if _, err = os.Stat(filepath.Join(state, "runner.lock")); err != nil {
				t.Fatal("stale main lock removed without operator review")
			}
		})
	}
}

// Opt in for the capacity regression: the synthetic SQLite database exceeds
// the old 512 MiB limit without allocating a giant Go byte slice.
func TestManagerLargeSnapshotCompressionRoundTrip(t *testing.T) {
	if os.Getenv("CPAMP_BACKUP_LARGE_TEST") != "1" {
		t.Skip("large disk/CPU regression is opt-in")
	}
	sources, root := fixture(t)
	dbFile := filepath.Join(root, "usage.sqlite")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	const payload = 520 << 20
	_, err = db.Exec("CREATE TABLE usage_events(value BLOB); INSERT INTO usage_events VALUES(zeroblob(?))", payload)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	dataKey := filepath.Join(root, "data.key")
	if err = os.WriteFile(dataKey, []byte("synthetic-manager-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources = append(sources, Source{Path: dbFile, Name: managerDatabaseName, SQLite: true, Required: true}, Source{Path: dataKey, Name: managerDataKeyName, Required: true})
	work := filepath.Join(root, "work")
	if err = os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	keyFile := filepath.Join(root, "recovery.key")
	if err = os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	file, _, err := Create(t.Context(), sources, work, key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() >= payload {
		t.Fatal("compression not applied", err)
	}
	restored := filepath.Join(root, "restored")
	if err = Restore(t.Context(), file, keyFile, restored); err != nil {
		t.Fatal(err)
	}
	db, err = openSQLite(filepath.Join(restored, filepath.FromSlash(managerDatabaseName)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var length int64
	if err = db.QueryRow("SELECT length(value) FROM usage_events").Scan(&length); err != nil || length != payload {
		t.Fatal("large database round trip failed", err)
	}
	t.Logf("synthetic %d MiB payload -> %.2f MiB encrypted backup (not a prediction of production ratio)", payload>>20, float64(info.Size())/(1<<20))
}
