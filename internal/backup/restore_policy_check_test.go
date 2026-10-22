package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueuedRebuildChecksHostApprovalBeforeDecrypting(t *testing.T) {
	for _, scenario := range []string{"missing-fields", "missing-digest", "invalid-digest", "changed-template", "ready", "database-only"} {
		t.Run(scenario, func(t *testing.T) {
			state, _, p := restoreFixture(t)
			mode := RebuildRestoreMode
			wantPhase, wantError := "preflight", ""
			switch scenario {
			case "missing-fields":
				p = PortalRestorePolicy{Database: p.Database, Container: p.Container, HealthURL: p.HealthURL}
				wantError = "manager_volume"
			case "missing-digest":
				p.ComposeSHA256 = ""
				wantError = "compose_sha256"
			case "invalid-digest":
				p.ComposeSHA256 = "由管理员填写已审核部署模板的SHA256"
				wantError = "compose_sha256"
			case "changed-template":
				if err := os.WriteFile(p.ComposeFile, []byte("services: {}"), 0o600); err != nil {
					t.Fatal(err)
				}
				wantError = "部署模板与后台批准的模板不一致"
			case "ready":
				wantPhase = "decrypt"
			case "database-only":
				p = PortalRestorePolicy{Database: p.Database, Container: p.Container, HealthURL: p.HealthURL}
				mode, wantPhase = DatabaseRestoreMode, "decrypt"
			}
			policyDir := t.TempDir()
			if err := os.Chmod(policyDir, 0o700); err != nil {
				t.Fatal(err)
			}
			policyFile := filepath.Join(policyDir, "restore.json")
			if err := writeJSON(policyFile, p); err != nil {
				t.Fatal(err)
			}
			// An invalid encrypted payload proves configuration rejection occurs
			// before decrypting it, rather than after a long archive operation.
			if err := RequestRestore(state, strings.NewReader(magic+"invalid encrypted payload"), make([]byte, 32), mode); err != nil {
				t.Fatal(err)
			}
			err := restoreTick(t.Context(), state, policyFile, time.Now().UTC(), func(PortalRestorePolicy, string) portalController {
				t.Fatal("failed preflight or decryption reached service controller")
				return nil
			})
			if err == nil || (wantError != "" && !strings.Contains(err.Error(), wantError)) {
				t.Fatalf("error: %v; want %s", err, wantError)
			}
			status, err := LoadRestoreStatus(state)
			if err != nil || status.Running || status.FailedPhase != wantPhase || status.Rollback != "" || DatabaseRecoveryPending(state) {
				t.Fatalf("status: %+v, %v", status, err)
			}
			if RestorePending(state) {
				t.Fatal("failed upload retained")
			}
			jobs, err := filepath.Glob(filepath.Join(state, ".restore-job-*"))
			if err != nil || len(jobs) != 0 {
				t.Fatalf("failed job retained: %v %v", jobs, err)
			}
			checkRestoreDB(t, p.Database, "live", false)
		})
	}
}

func TestRestoreInstallationCheckIsReadOnlyAndAcceptsFreshUpstreams(t *testing.T) {
	for _, scenario := range []string{"fresh-upstreams", "partial-upstreams", "wrong-volume", "missing-parent", "database-only"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, p := restoreFixture(t)
			mode := RebuildRestoreMode
			if scenario == "missing-parent" {
				p.ManagerKeyFile = filepath.Join(t.TempDir(), "missing", "cpamp-admin-key")
			}
			if scenario == "database-only" {
				mode = DatabaseRestoreMode
				p = PortalRestorePolicy{Database: p.Database, Container: p.Container, HealthURL: p.HealthURL}
			}
			var commands []string
			d := &dockerPortal{policy: p, rebuild: mode == RebuildRestoreMode, execute: func(_ context.Context, args ...string) ([]byte, error) {
				commands = append(commands, strings.Join(args, " "))
				switch args[0] {
				case "compose":
					if scenario == "database-only" || strings.Join(args[len(args)-3:], " ") != "config --format json" {
						t.Fatalf("unexpected compose command: %v", args)
					}
					volume := p.ManagerVolume
					if scenario == "wrong-volume" {
						volume = "unapproved-volume"
					}
					return json.Marshal(map[string]any{
						"services": map[string]any{p.ManagerService: map[string]any{
							"volumes":     []map[string]any{{"type": "volume", "source": "data", "target": "/data"}},
							"environment": map[string]string{"USAGE_DB_PATH": "/data/usage.sqlite", "CPA_MANAGER_DATA_KEY_PATH": "/data/data.key"},
						}},
						"volumes": map[string]any{"data": map[string]string{"name": volume}},
					})
				case "ps":
					if scenario == "partial-upstreams" {
						return []byte(p.Container + "\n" + p.UpstreamContainer + "\n"), nil
					}
					return []byte(p.Container + "\n"), nil
				case "inspect":
					if args[len(args)-1] != p.Container {
						t.Fatal("inspected absent upstream")
					}
					return json.Marshal(map[string]any{
						"State":  map[string]any{"Running": true, "Health": map[string]string{"Status": "healthy"}},
						"Mounts": []map[string]any{{"Source": filepath.Dir(p.Database), "Destination": "/data", "RW": true}},
					})
				default:
					t.Fatalf("installation check performed non-read-only operation: %v", args)
					return nil, nil
				}
			}}
			err := d.checkRestorePolicy(t.Context(), mode)
			wantSuccess := scenario == "fresh-upstreams" || scenario == "database-only"
			if (err == nil) != wantSuccess {
				t.Fatalf("check error: %v; commands: %v", err, commands)
			}
			if scenario == "missing-parent" && len(commands) != 0 {
				t.Fatal("missing target directory reached Docker")
			}
		})
	}
}
