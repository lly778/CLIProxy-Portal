package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/store"
)

type restoreService struct {
	stops, starts, health                        int
	stopFail, healthFail, alwaysFail, verifyFail bool
}

func (s *restoreService) Prepare(ctx context.Context, restored string) error {
	return validateRebuildContent(restored)
}
func (s *restoreService) OriginalDeployment(context.Context) (deploymentState, error) {
	return s.DesiredDeployment(), nil
}
func (s *restoreService) DesiredDeployment() deploymentState {
	return deploymentState{Images: map[string]string{"cpa": "cpa@sha256:" + strings.Repeat("a", 64), "cpamp": "cpamp@sha256:" + strings.Repeat("b", 64)}}
}
func (s *restoreService) RestoreDeployment(deploymentState) error { return nil }

func (s *restoreService) Verify(context.Context, PortalRestorePolicy) error {
	if s.verifyFail {
		return errors.New("mount mismatch")
	}
	return nil
}
func (s *restoreService) Stop(context.Context) error {
	s.stops++
	if s.stopFail && s.stops == 1 {
		return errors.New("stop failed")
	}
	return nil
}
func (s *restoreService) Start(context.Context) error { s.starts++; return nil }
func (s *restoreService) Healthy(context.Context) error {
	s.health++
	if s.alwaysFail || (s.healthFail && s.health == 1) {
		return errors.New("health failed")
	}
	return nil
}

func restoreTestDB(t *testing.T, file, name string) {
	t.Helper()
	st, err := store.Open(file, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO users(id,phone,name,password_hash,role,status,created_at_ms,updated_at_ms) VALUES('admin','13800138000','` + name + `','test-hash','admin','approved',1,1)`,
		`INSERT INTO api_keys(id,user_id,key_hash,last_four,alias,status,issued_at_ms) VALUES('key','admin','` + name + `-hash','test','alias','active',1)`,
		`INSERT INTO sessions(token_hash,user_id,created_at_ms,last_seen_at_ms,expires_at_ms) VALUES('old-session','admin',1,1,99999999)`,
		`INSERT INTO password_resets(code_hash,user_id,expires_at_ms,created_at_ms) VALUES('old-reset','admin',99999999,1)`,
		`INSERT INTO sync_jobs(user_id,action,key_hash,next_at_ms,created_at_ms) VALUES('admin','revoke','old-key',1,1)`,
		`INSERT INTO settings(key,value) VALUES('upstream_card_layout:presets','["` + name + `"]')`,
	} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
}

func restoreFixture(t *testing.T) (string, string, PortalRestorePolicy) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	data := filepath.Join(root, "data")
	upstream := filepath.Join(root, "upstream")
	restored := filepath.Join(root, "restored")
	for _, dir := range []string{state, data, upstream, filepath.Join(root, "secrets"), filepath.Join(upstream, "secrets"), filepath.Join(restored, "upstream/secrets"), filepath.Join(upstream, "auths"), filepath.Join(restored, "portal/data"), filepath.Join(restored, "portal/secrets"), filepath.Join(restored, "upstream/cliproxyapi/auths")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := PortalRestorePolicy{Database: filepath.Join(data, "portal.db"), Container: "portal", HealthURL: "http://127.0.0.1:18080/healthz", UpstreamConfig: filepath.Join(upstream, "config.yaml"), UpstreamContainer: "cpa", UpstreamHealthURL: "http://127.0.0.1:8317/", ManagerContainer: "manager", ManagerHealthURL: "http://127.0.0.1:18317/health", Auths: filepath.Join(upstream, "auths")}
	p.ComposeFile = filepath.Join(upstream, "compose.yaml")
	p.ProjectName = "cpamp"
	p.UpstreamService = "cpa"
	p.ManagerService = "cpamp"
	p.ManagerKeyFile = filepath.Join(upstream, "secrets/cpamp-admin-key")
	p.CPAKeyFile = filepath.Join(upstream, "secrets/cpa-management-key")
	p.PortalManagerKeyFile = filepath.Join(root, "secrets/cpamp_admin_key")
	p.UpstreamRepository = "cpa"
	p.ManagerRepository = "cpamp"
	managerDir := filepath.Join(root, "manager-data")
	if err := os.MkdirAll(managerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(restored, "upstream/cpamp"), 0o700); err != nil {
		t.Fatal(err)
	}
	p.ManagerDatabase = filepath.Join(managerDir, "usage.sqlite")
	p.ManagerDataKey = filepath.Join(managerDir, "data.key")
	p.ManagerVolume = "cpamp-data"
	managerTestDB(t, p.ManagerDatabase, "live-manager")
	managerTestDB(t, filepath.Join(restored, filepath.FromSlash(managerDatabaseName)), "backup-manager")
	restoreTestDB(t, p.Database, "live")
	restoreTestDB(t, filepath.Join(restored, "portal/data/portal.db"), "backup")
	for file, content := range map[string]string{
		filepath.Join(root, "app.key"): strings.Repeat("a", 64),
		p.UpstreamConfig:               "port: 8317\nauth-dir: /root/.cli-proxy-api\napi-keys: [live-key]\noauth-excluded-models: {}\n",
		filepath.Join(restored, "upstream/cliproxyapi/config.yaml"):            "port: 8317\nauth-dir: /root/.cli-proxy-api\napi-keys: [backup-key]\noauth-excluded-models: {codex: [gpt-disabled]}\noauth-model-alias: {codex: [{name: gpt-real, alias: gpt-alias}]}\n",
		filepath.Join(p.Auths, "live-only.json"):                               `{"refresh_token":"live-only"}`,
		filepath.Join(restored, "upstream/cliproxyapi/auths/backup-only.json"): `{"refresh_token":"backup-only"}`,
	} {
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range append(p.targets(RebuildRestoreMode)[3:8], p.targets(RebuildRestoreMode)[9:]...) {
		live := "live-value-for-" + target.Archive
		archived := "backup-value-for-" + target.Archive
		if target.Archive == "portal/secrets/cpamp_admin_key" {
			live = "live-value-for-upstream/secrets/cpamp-admin-key"
			archived = "backup-value-for-upstream/secrets/cpamp-admin-key"
		}
		if err := os.WriteFile(target.Path, []byte(live), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(restored, filepath.FromSlash(target.Archive)), []byte(archived), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return state, restored, p
}

func checkRestoreDB(t *testing.T, file, name string, cleared bool) {
	t.Helper()
	db, err := openSQLite(file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got, hash, layout string
	if err = db.QueryRow("SELECT name FROM users WHERE id='admin'").Scan(&got); err != nil || got != name {
		t.Fatalf("name=%s err=%v", got, err)
	}
	if err = db.QueryRow("SELECT key_hash FROM api_keys WHERE id='key'").Scan(&hash); err != nil || hash != name+"-hash" {
		t.Fatalf("API Key record=%s err=%v", hash, err)
	}
	if err = db.QueryRow("SELECT value FROM settings WHERE key='upstream_card_layout:presets'").Scan(&layout); err != nil || layout != `["`+name+`"]` {
		t.Fatalf("layout=%s err=%v", layout, err)
	}
	for _, table := range []string{"sessions", "password_resets", "sync_jobs"} {
		var count int
		if err = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if (cleared && count != 0) || (!cleared && count != 1) {
			t.Fatalf("%s count=%d", table, count)
		}
	}
}

func TestPortalAndUpstreamRestoreSucceedsTogether(t *testing.T) {
	state, restored, p := restoreFixture(t)
	svc := &restoreService{}
	backupConfig, _ := os.ReadFile(filepath.Join(restored, "upstream/cliproxyapi/config.yaml"))
	rollback, err := replacePortalDatabase(t.Context(), state, restored, p, svc, RebuildRestoreMode)
	if err != nil {
		t.Fatal(err)
	}
	if svc.stops != 1 || svc.starts != 1 || svc.health != 1 {
		t.Fatalf("calls %+v", svc)
	}
	checkRestoreDB(t, p.Database, "backup", true)
	checkRestoreDB(t, filepath.Join(state, rollback, "0"), "live", false)
	checkManagerRow(t, p.ManagerDatabase, "backup-manager")
	checkManagerRow(t, filepath.Join(state, rollback, "8"), "live-manager")
	config, _ := os.ReadFile(p.UpstreamConfig)
	if !bytes.Equal(config, backupConfig) {
		t.Fatal("upstream configuration not restored")
	}
	if _, err = os.Stat(filepath.Join(p.Auths, "live-only.json")); !os.IsNotExist(err) {
		t.Fatal("auths merged instead of replaced")
	}
	if _, err = os.Stat(filepath.Join(p.Auths, "backup-only.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(state, rollback, "2/live-only.json")); err != nil {
		t.Fatal(err)
	}
	secret, _ := os.ReadFile(filepath.Join(filepath.Dir(state), "app.key"))
	if string(secret) != strings.Repeat("a", 64) || DatabaseRecoveryPending(state) {
		t.Fatal("secret changed or transaction retained")
	}
	for _, target := range append(p.targets(RebuildRestoreMode)[3:8], p.targets(RebuildRestoreMode)[9:]...) {
		got, _ := os.ReadFile(target.Path)
		want, _ := os.ReadFile(filepath.Join(restored, filepath.FromSlash(target.Archive)))
		if !bytes.Equal(got, want) {
			t.Fatalf("deployment/secret not restored: %s", target.Archive)
		}
	}
}

func TestDatabaseOnlyRestoreDoesNotRequireOrChangeUpstream(t *testing.T) {
	state, restored, p := restoreFixture(t)
	before := map[string][]byte{}
	for _, target := range p.targets(RebuildRestoreMode)[1:] {
		if target.Kind != "directory" {
			before[target.Path], _ = os.ReadFile(target.Path)
		}
	}
	if err := os.RemoveAll(filepath.Join(restored, "upstream")); err != nil {
		t.Fatal(err)
	}
	if _, err := replacePortalDatabase(t.Context(), state, restored, p, &restoreService{}, DatabaseRestoreMode); err != nil {
		t.Fatal(err)
	}
	checkRestoreDB(t, p.Database, "backup", true)
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("ordinary restore touched upstream: %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Auths, "live-only.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreValidationNeverStopsLiveServices(t *testing.T) {
	for _, mode := range []string{"config", "schema", "admin", "auths", "mount", "manager-db", "data-key", "archive-references"} {
		t.Run(mode, func(t *testing.T) {
			state, restored, p := restoreFixture(t)
			svc := &restoreService{}
			switch mode {
			case "config":
				_ = os.Remove(filepath.Join(restored, "upstream/cliproxyapi/config.yaml"))
			case "schema", "admin":
				db, _ := sql.Open("sqlite", filepath.Join(restored, "portal/data/portal.db"))
				query := "DROP TABLE sessions"
				if mode == "admin" {
					query = "UPDATE users SET status='disabled'"
				}
				_, err := db.Exec(query)
				_ = db.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "auths":
				_ = os.RemoveAll(filepath.Join(restored, "upstream/cliproxyapi/auths"))
			case "mount":
				svc.verifyFail = true
			case "manager-db":
				_ = os.WriteFile(filepath.Join(restored, filepath.FromSlash(managerDatabaseName)), []byte("invalid"), 0o600)
			case "data-key":
				_ = os.Remove(filepath.Join(restored, filepath.FromSlash(managerDataKeyName)))
			case "archive-references":
				db, err := sql.Open("sqlite", filepath.Join(restored, filepath.FromSlash(managerDatabaseName)))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("CREATE TABLE usage_archive_runs(id TEXT); INSERT INTO usage_archive_runs VALUES('missing-archive')")
				_ = db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := replacePortalDatabase(t.Context(), state, restored, p, svc, RebuildRestoreMode); err == nil {
				t.Fatal("unsafe restore accepted")
			}
			if svc.stops != 0 || svc.starts != 0 {
				t.Fatal("validation stopped live service")
			}
			checkRestoreDB(t, p.Database, "live", false)
			config, _ := os.ReadFile(p.UpstreamConfig)
			if !bytes.Contains(config, []byte("live-key")) {
				t.Fatal("configuration changed")
			}
			if DatabaseRecoveryPending(state) {
				t.Fatal("validation retained transaction")
			}
		})
	}
}

func TestFailedRestoreRollsBackDatabaseConfigAndAuths(t *testing.T) {
	for _, mode := range []string{"stop", "health", "interrupted-rollback"} {
		t.Run(mode, func(t *testing.T) {
			state, restored, p := restoreFixture(t)
			before := map[string][]byte{}
			for _, target := range append(p.targets(RebuildRestoreMode)[3:8], p.targets(RebuildRestoreMode)[9:]...) {
				before[target.Path], _ = os.ReadFile(target.Path)
			}
			svc := &restoreService{stopFail: mode == "stop", healthFail: mode == "health", alwaysFail: mode == "interrupted-rollback"}
			_, err := replacePortalDatabase(t.Context(), state, restored, p, svc, RebuildRestoreMode)
			if err == nil {
				t.Fatal("failure reported success")
			}
			if mode == "interrupted-rollback" {
				if !DatabaseRecoveryPending(state) {
					t.Fatal("failed rollback discarded recovery journal")
				}
				svc.alwaysFail = false
				if err = recoverDatabaseTransaction(t.Context(), state, p, svc); err != nil {
					t.Fatal(err)
				}
			}
			checkRestoreDB(t, p.Database, "live", false)
			checkManagerRow(t, p.ManagerDatabase, "live-manager")
			config, _ := os.ReadFile(p.UpstreamConfig)
			if !bytes.Contains(config, []byte("live-key")) {
				t.Fatal("configuration not rolled back")
			}
			if _, err = os.Stat(filepath.Join(p.Auths, "live-only.json")); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(p.Auths, "backup-only.json")); !os.IsNotExist(err) {
				t.Fatal("new auths survived rollback")
			}
			if DatabaseRecoveryPending(state) {
				t.Fatal("completed rollback left journal")
			}
			for path, want := range before {
				got, _ := os.ReadFile(path)
				if !bytes.Equal(got, want) {
					t.Fatalf("deployment/secret not rolled back: %s", path)
				}
			}
		})
	}
}

func TestQueuedDatabaseRestoreAndOldConsentRejection(t *testing.T) {
	state, restored, p := restoreFixture(t)
	policyFile := filepath.Join(t.TempDir(), "restore.json")
	if err := os.Chmod(filepath.Dir(policyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(policyFile, p); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	sources := []Source{{Path: filepath.Join(restored, "portal/data/portal.db"), Name: "portal/data/portal.db", SQLite: true, Required: true}, {Path: filepath.Join(restored, "upstream/cliproxyapi/config.yaml"), Name: "upstream/cliproxyapi/config.yaml", Required: true}, {Path: filepath.Join(restored, "upstream/cliproxyapi/auths"), Name: "upstream/cliproxyapi/auths", Directory: true, Required: true}}
	key := bytes.Repeat([]byte{7}, 32)
	archive, _, err := Create(t.Context(), sources, work, key)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(archive)
	if err = RequestRestore(state, bytes.NewReader(content), key, DatabaseRestoreMode); err != nil {
		t.Fatal(err)
	}
	if err = RequestRestore(state, bytes.NewReader(content), key, DatabaseRestoreMode); err == nil {
		t.Fatal("duplicate allowed")
	}
	svc := &restoreService{}
	run := func() error {
		return restoreTick(t.Context(), state, policyFile, time.Now(), func(PortalRestorePolicy, string) portalController { return svc })
	}
	if err = run(); err != nil {
		t.Fatal(err)
	}
	status, err := LoadRestoreStatus(state)
	if err != nil || status.Running || status.FinishedAt.IsZero() || status.Rollback == "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	checkRestoreDB(t, p.Database, "backup", true)
	if RestorePending(state) {
		t.Fatal("completed request retained")
	}
	if err = RequestRestore(state, bytes.NewReader(content), bytes.Repeat([]byte{3}, 32), DatabaseRestoreMode); err != nil {
		t.Fatal(err)
	}
	before := svc.stops
	if err = run(); err == nil || svc.stops != before {
		t.Fatal("wrong key touched live service")
	}
	if err = RequestRestore(state, bytes.NewReader(content), key, DatabaseRestoreMode); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(state, "restore-pending/request.json"))
	if err = run(); err == nil || svc.stops != before {
		t.Fatal("old isolated-directory consent reused for overwrite")
	}
	if err = RequestRestore(state, strings.NewReader("plaintext"), key, DatabaseRestoreMode); err == nil {
		t.Fatal("plaintext accepted")
	}
}
