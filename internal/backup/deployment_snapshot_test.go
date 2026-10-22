package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func deploymentSourcesForTest(p PortalRestorePolicy) []Source {
	var sources []Source
	for _, target := range p.targets(RebuildRestoreMode) {
		sources = append(sources, Source{Path: target.Path, Name: target.Archive, Required: true,
			Directory: target.Kind == "directory", SQLite: target.Kind == "database" || target.Kind == "manager-database"})
	}
	return sources
}

func TestDeploymentConfigSurvivesEncryptedBackupWithoutChangingHostPolicy(t *testing.T) {
	_, _, p := restoreFixture(t)
	if err := os.Remove(filepath.Join(p.Auths, "live-only.json")); err != nil {
		t.Fatal(err)
	}
	sources := deploymentSourcesForTest(p)
	work := t.TempDir()
	snapshots, err := snapshotDeploymentConfig(sources, &p, work)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{5}, 32)
	keyFile := filepath.Join(t.TempDir(), "recovery.key")
	if err = os.WriteFile(keyFile, []byte(strings.Repeat("05", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	archive, manifest, err := Create(t.Context(), append(append([]Source{}, sources...), snapshots...), work, key)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "review")
	if err = Restore(t.Context(), archive, keyFile, output); err != nil {
		t.Fatal(err)
	}
	var restoredPolicy PortalRestorePolicy
	if err = readJSON(filepath.Join(output, filepath.FromSlash(restorePolicySnapshotName)), &restoredPolicy); err != nil || !reflect.DeepEqual(restoredPolicy, p) {
		t.Fatalf("policy not preserved: %+v, %v", restoredPolicy, err)
	}
	var restoredSources []Source
	if err = readJSON(filepath.Join(output, filepath.FromSlash(sourcesSnapshotName)), &restoredSources); err != nil || !reflect.DeepEqual(restoredSources, sources) {
		t.Fatalf("sources not preserved: %+v, %v", restoredSources, err)
	}
	for _, name := range []string{restorePolicySnapshotName, sourcesSnapshotName} {
		found := false
		for _, entry := range manifest.Entries {
			if entry.Name == name && entry.SHA256 != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("configuration missing from authenticated manifest: %s", name)
		}
	}
	after, err := os.ReadFile(p.ComposeFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("extracting metadata changed the host deployment")
	}
	// A foreign host path is descriptive metadata, never a local restore target.
	// Reading it must also work on Windows when the source backup is from Linux.
	restoredPolicy.Database = "/root/original-server/data/portal.db"
	if err = writeJSON(filepath.Join(output, filepath.FromSlash(restorePolicySnapshotName)), restoredPolicy); err != nil {
		t.Fatal(err)
	}
	if err = validateDeploymentSnapshot(output); err != nil {
		t.Fatalf("foreign host metadata interpreted as a local target: %v", err)
	}
}

func TestCompleteBackupRejectsMissingSourcesAndChangedTemplate(t *testing.T) {
	for _, scenario := range []string{"missing", "optional", "wrong-path", "reserved", "changed-template", "changed-after-snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, p := restoreFixture(t)
			sources := deploymentSourcesForTest(p)
			work := t.TempDir()
			switch scenario {
			case "missing":
				sources = sources[1:]
			case "optional":
				sources[0].Required = false
			case "wrong-path":
				sources[0].Path += ".other"
			case "reserved":
				sources = append(sources, Source{Name: restorePolicySnapshotName})
			case "changed-template":
				if err := os.WriteFile(p.ComposeFile, []byte("unapproved"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			snapshots, err := snapshotDeploymentConfig(sources, &p, work)
			if scenario != "changed-after-snapshot" {
				if err == nil {
					t.Fatal("incomplete/unapproved rebuild backup accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(p.ComposeFile, []byte("changed-during-backup"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err = Create(t.Context(), append(sources, snapshots...), work, bytes.Repeat([]byte{1}, 32))
			if err == nil || !strings.Contains(err.Error(), "SHA256") {
				t.Fatalf("template race was not rejected: %v", err)
			}
		})
	}
}
