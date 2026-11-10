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

func agyFixture(t *testing.T) (hostInstaller, *[]string, *agyCredentials) {
	t.Helper()
	h, commands := testHostInstaller(t)
	a := &agyCredentials{Secret: strings.Repeat("agy-private-key-", 4), OAuth: json.RawMessage(`{"id_token_email":"new@example.test","token":{"refresh_token":"private-refresh-token"}}`), InstallationID: "test-installation", MaxSessions: 3, MemoryMiB: 512}
	for path, data := range map[string][]byte{
		agySecretPath: []byte(a.Secret), defaultPortalRoot + "/secrets/portal_agy_secret": []byte(a.Secret),
		agyTokenPath: a.OAuth, agyHome + "/agy/installation_id": []byte(a.InstallationID),
		"/etc/cliproxy-agy/runtime.env": []byte("PORTAL_AGY_EMBEDDED=true\nAGY_MAX_SESSIONS=3\nAGY_CONTAINER_MEMORY_MIB=512\n"),
	} {
		if err := h.write(path, data, 0600, false); err != nil {
			t.Fatal(err)
		}
	}
	h.agyDownload = func(context.Context) error {
		return h.write("/opt/cliproxy-agy/bin/agy", []byte("official-test-fixture"), 0755, false)
	}
	return h, commands, a
}

func TestAGYBackupEncryptsOnlyCredentialsAndScalarSettings(t *testing.T) {
	h, _, a := agyFixture(t)
	// These existing files must never enter the AGY credential snapshot.
	for _, path := range []string{agyHome + "/portal-sessions.db", agyHome + "/agy/brain/chat/image.png", "/opt/cliproxy-agy/bin/agy"} {
		if err := h.write(path, []byte("excluded-session-or-program"), 0600, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := snapshotAGY(h.path("/etc"), h.path(agyHome), h.path(defaultPortalRoot))
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != a.Secret || got.InstallationID != a.InstallationID || got.MaxSessions != 3 || got.MemoryMiB != 512 || !bytes.Equal(got.OAuth, a.OAuth) {
		t.Fatal("AGY snapshot incomplete; secret values withheld")
	}
	sources, _ := fixture(t)
	work := t.TempDir()
	source, err := writeCredentialSnapshot(credentialSnapshot{Version: credentialsSnapshotVersion, Repository: "owner/private", GitHubToken: strings.Repeat("g", 40), AGY: got}, work)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{8}, 32)
	archive, manifest, err := Create(t.Context(), archiveSources(t, append(sources, source)), work, key)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err = os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "restore")
	if err = Restore(t.Context(), archive, keyFile, output); err != nil {
		t.Fatal(err)
	}
	v, err := readCredentials(output)
	if err != nil || v.AGY == nil || v.AGY.Secret != a.Secret || !bytes.Equal(v.AGY.OAuth, a.OAuth) {
		t.Fatal("encrypted AGY roundtrip failed; values withheld")
	}
	raw, _ := os.ReadFile(archive)
	for _, private := range []string{a.Secret, "private-refresh-token", "excluded-session-or-program"} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("plaintext or excluded data in backup")
		}
	}
	for _, e := range manifest.Entries {
		if strings.Contains(e.Name, "agy/") || strings.Contains(e.Name, "sessions") || strings.Contains(e.Name, ".png") {
			t.Fatal("AGY session or program archived")
		}
	}
}

func TestAGYSnapshotRejectsPartialCredentialsAndSymlinks(t *testing.T) {
	h, _, _ := agyFixture(t)
	if err := h.write(defaultPortalRoot+"/secrets/portal_agy_secret", []byte(strings.Repeat("different-secret", 4)), 0600, false); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotAGY(h.path("/etc"), h.path(agyHome), h.path(defaultPortalRoot)); err == nil {
		t.Fatal("mismatched service secrets accepted")
	}
	if err := os.Remove(h.path(agyTokenPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.path(agySecretPath), h.path(agyTokenPath)); err == nil {
		if _, err := privateAGYFile(h.path(agyTokenPath), false); err == nil {
			t.Fatal("credential symlink accepted")
		}
	}
}

func TestAGYSnapshotRequiresCompleteContainerSettings(t *testing.T) {
	for _, settings := range []string{
		"AGY_MAX_SESSIONS=3\nAGY_CONTAINER_MEMORY_MIB=512\n",
		"PORTAL_AGY_EMBEDDED=true\nAGY_MAX_SESSIONS=3\n",
		"PORTAL_AGY_EMBEDDED=true\nAGY_CONTAINER_MEMORY_MIB=512\n",
		"PORTAL_AGY_EMBEDDED=true\nAGY_MAX_SESSIONS=3\nAGY_MAX_SESSIONS=4\nAGY_CONTAINER_MEMORY_MIB=512\n",
	} {
		t.Run(settings, func(t *testing.T) {
			h, _, _ := agyFixture(t)
			if err := h.write("/etc/cliproxy-agy/runtime.env", []byte(settings), 0600, false); err != nil {
				t.Fatal(err)
			}
			if _, err := snapshotAGY(h.path("/etc"), h.path(agyHome), h.path(defaultPortalRoot)); err == nil {
				t.Fatal("incomplete or ambiguous container settings accepted")
			}
		})
	}
}

func TestAGYRebuildUsesTrustedAssetsAndInvalidatesOldSessions(t *testing.T) {
	h, commands, a := agyFixture(t)
	if err := h.write(agyConfigPath, []byte(`{"max_sessions":3,"memory_mib":512,"logged_in":true}`), 0600, false); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.path(agyHome+"/portal-sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions(id TEXT,ready INTEGER); INSERT INTO sessions VALUES('old-chat',1); CREATE TABLE responses(id TEXT); INSERT INTO responses VALUES('old-response');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = h.installAGY(t.Context()); err != nil {
		t.Fatal(err)
	}
	dockerfile, _ := os.ReadFile(h.path(defaultPortalRoot + "/ops/agy/Dockerfile.runtime"))
	env, _ := os.ReadFile(h.path("/etc/cliproxy-agy/runtime.env"))
	if !bytes.Contains(dockerfile, []byte("debian:bookworm-slim")) || !bytes.Contains(env, []byte("PORTAL_AGY_EMBEDDED=true")) || !bytes.Contains(env, []byte("AGY_CONTAINER_MEMORY_MIB=512")) {
		t.Fatal("AGY not rebuilt with trusted settings")
	}
	if strings.Contains(strings.Join(*commands, "\n"), a.Secret) {
		t.Fatal("service key exposed in command")
	}
	for _, expected := range []string{"chown -R --no-dereference 10001:10001 " + agyHome, "chown 10001:10001"} {
		if !strings.Contains(strings.Join(*commands, "\n"), expected) {
			t.Fatal("missing AGY rebuild step", expected)
		}
	}
	db, _ = sql.Open("sqlite", h.path(agyHome+"/portal-sessions.db"))
	defer db.Close()
	var ready, responses int
	_ = db.QueryRow(`SELECT ready FROM sessions`).Scan(&ready)
	_ = db.QueryRow(`SELECT COUNT(*) FROM responses`).Scan(&responses)
	if ready != 0 || responses != 0 {
		t.Fatal("restored credentials reused previous session")
	}
	if err = h.write(agyConfigPath, []byte(`{"max_sessions":3,"memory_mib":512,"logged_in":false}`), 0600, false); err != nil {
		t.Fatal(err)
	}
	if err = h.installAGY(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(h.path(agyTokenPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logged-out backup retained target login")
	}
}

func TestAGYRestoreOnlyAddsFixedCredentialTargets(t *testing.T) {
	flags := restoredCredentials{AGY: true, AGYOAuth: true, AGYInstallation: true}
	targets := credentialTargets("/unused", flags)
	if len(targets) != 5 {
		t.Fatal("AGY restore targets missing")
	}
	for _, target := range targets {
		if target.Kind != "file" || strings.Contains(target.Path, "session") || strings.Contains(target.Path, "/bin/") {
			t.Fatal("restoring program or session instead of credentials")
		}
	}
	v := credentialSnapshot{Version: credentialsSnapshotVersion, Repository: "owner/private", GitHubToken: strings.Repeat("g", 40)}
	if v.validate() != nil {
		t.Fatal("new-format backup without installed AGY rejected")
	}
	v.Version = 1
	if v.validate() == nil {
		t.Fatal("old backup format accepted")
	}
}

func TestAGYHostPreparationIsNotAnInstalledAccount(t *testing.T) {
	h, commands := testHostInstaller(t)
	if err := h.prepareAGYHost(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := snapshotAGY(h.path("/etc"), h.path(agyHome), h.path(defaultPortalRoot))
	if err != nil || v != nil {
		t.Fatal("empty prepared directories marked as an installed AGY account")
	}
	for _, cmd := range *commands {
		if strings.Contains(cmd, "restart") || strings.Contains(cmd, "backup tick") {
			t.Fatal("host preparation started service or backup")
		}
	}
}

func TestAGYHostRebuildFailureRemainsPendingAndRetries(t *testing.T) {
	h, _, a := agyFixture(t)
	if err := h.write(agyConfigPath, []byte(`{"max_sessions":3,"memory_mib":512,"logged_in":true}`), 0600, false); err != nil {
		t.Fatal(err)
	}
	state := h.path(defaultPortalRoot + "/data/backup")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := writeCredentialSnapshot(credentialSnapshot{Version: credentialsSnapshotVersion, Repository: "owner/private", GitHubToken: strings.Repeat("g", 40), AGY: a}, state)
	if err != nil {
		t.Fatal(err)
	}
	// prepareHostRestore reads an extracted archive; keep it in the fixture.
	restored := t.TempDir()
	if err := os.MkdirAll(filepath.Join(restored, "portal/backup"), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(state, "backup-credentials.json"))
	if err := os.WriteFile(filepath.Join(restored, credentialsSnapshotName), data, 0600); err != nil {
		t.Fatal(err)
	}
	p, _ := defaultRestorePolicy()
	if err = prepareHostRestore(state, p, restored); err != nil {
		t.Fatal(err)
	}
	if err = markHostRestoreReady(state); err != nil {
		t.Fatal(err)
	}
	originalRun := h.run
	h.run = func(ctx context.Context, args ...string) error {
		if strings.Join(args, " ") == "systemctl enable --now cliproxy-portal-log-metrics.timer" {
			return errors.New("fixture startup failure")
		}
		return originalRun(ctx, args...)
	}
	if err = resumeHostRestoreWith(t.Context(), state, h); err == nil {
		t.Fatal("AGY startup failure reported as success")
	}
	s, err := LoadRestoreStatus(state)
	if err != nil || s.Phase != "host_pending" || !HostRecoveryPending(state) {
		t.Fatal("AGY failure lost durable pending plan")
	}
	h.run = originalRun
	if err = resumeHostRestoreWith(t.Context(), state, h); err != nil {
		t.Fatal(err)
	}
	s, err = LoadRestoreStatus(state)
	if err != nil || s.Phase != "complete" || HostRecoveryPending(state) {
		t.Fatal("AGY retry did not finish")
	}
}

func TestAGYPortalRestoreRegeneratesPrivateEndpointAndSecretMount(t *testing.T) {
	h, _, _ := agyFixture(t)
	for _, name := range []string{"compose.prebuilt.yaml", "compose.gateway.yaml"} {
		if err := h.write(defaultPortalRoot+"/"+name, []byte("services: {}\n"), 0600, false); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := defaultRestorePolicy()
	files := defaultPortalRoot + "/compose.prebuilt.yaml," + defaultPortalRoot + "/compose.gateway.yaml"
	d := dockerPortal{policy: p, installer: &h, hostAGY: true, execute: func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "inspect" {
			return json.Marshal(portalInspection{Labels: map[string]string{"com.docker.compose.project.working_dir": defaultPortalRoot, "com.docker.compose.service": "cliproxy-portal", "com.docker.compose.project": "cliproxy-portal", "com.docker.compose.project.config_files": files}})
		}
		if args[0] == "network" {
			return []byte("172.23.0.1"), nil
		}
		return nil, nil
	}}
	if err := d.recreatePortal(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.path(defaultPortalRoot + "/ops/compose.restore-monitoring.json"))
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Services map[string]struct {
			Environment map[string]string
			Volumes     []struct {
				Source, Target string
				ReadOnly       bool `json:"read_only"`
			}
		}
	}
	if json.Unmarshal(data, &overlay) != nil {
		t.Fatal("invalid compose overlay")
	}
	svc := overlay.Services["cliproxy-portal"]
	if svc.Environment["PORTAL_AGY_BASE_URL"] != "http://127.0.0.1:18320" || svc.Environment["PORTAL_AGY_EMBEDDED"] != "true" || len(svc.Volumes) != 3 || svc.Volumes[0].Target != "/run/secrets/portal_agy_secret" || !svc.Volumes[0].ReadOnly {
		t.Fatal("embedded AGY routing or private mounts missing")
	}
}
