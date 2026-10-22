package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	portalops "cliproxy-portal/ops"
)

func testHostInstaller(t *testing.T) (hostInstaller, *[]string) {
	t.Helper()
	commands := &[]string{}
	h := hostInstaller{root: t.TempDir(), run: func(_ context.Context, args ...string) error {
		*commands = append(*commands, strings.Join(args, " "))
		return nil
	}}
	return h, commands
}

func TestHostInstallerBuildsFreshQueueAndPreservesPolicy(t *testing.T) {
	h, commands := testHostInstaller(t)
	if err := h.install(t.Context()); err != nil {
		t.Fatal(err)
	}
	var p PortalRestorePolicy
	if err := readJSON(h.path(defaultPolicyFile), &p); err != nil || p.Bootstrap != autoBootstrap || !p.defaultHostLayout() {
		t.Fatalf("policy=%+v, %v", p, err)
	}
	if err := p.validate(RebuildRestoreMode); runtime.GOOS == "linux" && err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.path(defaultPortalRoot + "/data/backup")); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"docker compose version", "python3 --version", "systemctl daemon-reload", "systemctl enable --now cliproxy.slice", "systemctl enable --now cliproxy-portal-backup.timer"} {
		if !strings.Contains(strings.Join(*commands, "\n"), expected) {
			t.Fatal("missing step", expected)
		}
	}
	for _, cmd := range *commands {
		if strings.Contains(cmd, "backup tick") || strings.Contains(cmd, "docker stop") {
			t.Fatal("installation generated backup or stopped service", cmd)
		}
	}
	p.HealthURL = "http://127.0.0.1:19999/healthz"
	if err := writeJSON(h.path(defaultPolicyFile), p); err != nil {
		t.Fatal(err)
	}
	if err := h.install(t.Context()); err != nil {
		t.Fatal(err)
	}
	var kept PortalRestorePolicy
	_ = readJSON(h.path(defaultPolicyFile), &kept)
	if kept.HealthURL != p.HealthURL {
		t.Fatal("existing policy overwritten")
	}
	// Bootstrap data is a compiled asset, never taken from the uploaded unit.
	want, _ := portalops.HostAssets.ReadFile("collect-esxi-metrics.py")
	got, _ := os.ReadFile(h.path(defaultPortalRoot + "/ops/collect-esxi-metrics.py"))
	if !bytes.Equal(got, want) {
		t.Fatal("collector differs from built-in asset")
	}
}

func TestHostInstallationRetryDoesNotClaimDataRollback(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := defaultRestorePolicy()
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareHostRestore(state, p, credentialDirectory(t)); err != nil {
		t.Fatal(err)
	}
	if err := markHostRestoreReady(state); err != nil {
		t.Fatal(err)
	}
	h, commands := testHostInstaller(t)
	fail := true
	h.run = func(_ context.Context, args ...string) error {
		*commands = append(*commands, strings.Join(args, " "))
		if fail && args[0] == "systemctl" {
			return errors.New("simulated systemd failure")
		}
		return nil
	}
	if err := resumeHostRestoreWith(t.Context(), state, h); err == nil {
		t.Fatal("installation failure ignored")
	}
	status, _ := LoadRestoreStatus(state)
	if status.Running || status.FailedPhase != "install_host" || !HostRecoveryPending(state) || !strings.Contains(status.Message, "数据已恢复") {
		t.Fatalf("status=%+v", status)
	}
	fail = false
	if err := resumeHostRestoreWith(t.Context(), state, h); err != nil {
		t.Fatal(err)
	}
	status, _ = LoadRestoreStatus(state)
	if !status.Successful() || HostRecoveryPending(state) {
		t.Fatalf("retry did not finish: %+v", status)
	}
}

func TestUncommittedHostPlanNeverInstallsTasks(t *testing.T) {
	state := t.TempDir()
	_ = os.Chmod(state, 0o700)
	p, _ := defaultRestorePolicy()
	if err := prepareHostRestore(state, p, credentialDirectory(t)); err != nil {
		t.Fatal(err)
	}
	h, commands := testHostInstaller(t)
	if err := resumeHostRestoreWith(t.Context(), state, h); err != nil {
		t.Fatal(err)
	}
	if len(*commands) != 0 || HostRecoveryPending(state) {
		t.Fatal("uncommitted task was installed")
	}
	status, _ := LoadRestoreStatus(state)
	if status.Successful() {
		t.Fatal("aborted data restore marked successful")
	}
}

func TestInstallerRejectsSymlinkTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux permissions and symlinks")
	}
	h, _ := testHostInstaller(t)
	outside := t.TempDir()
	if err := os.MkdirAll(h.path("/etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, h.path("/etc/systemd")); err != nil {
		t.Fatal(err)
	}
	if err := h.write("/etc/systemd/system/malicious.service", []byte("bad"), 0o644, false); err == nil {
		t.Fatal("link followed")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("wrote outside root")
	}
}

func TestBootstrapEnvRejectsExecutableOrStructuralChanges(t *testing.T) {
	p, _ := defaultRestorePolicy()
	valid := "COMPOSE_PROJECT_NAME=cpamp\nCPA_IMAGE=eceasy/cli-proxy-api@sha256:" + strings.Repeat("a", 64) + "\nCPAMP_IMAGE=seakee/cpa-manager-plus@sha256:" + strings.Repeat("b", 64) + "\nCPA_PORT=8317\nCPAMP_PORT=18317\n"
	if err := validateBootstrapEnv(valid, p); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{valid + "DOCKER_HOST=remote\n", strings.Replace(valid, "CPA_PORT=8317", "CPA_PORT=22", 1), strings.Replace(valid, "CPA_PORT=8317", "CPA_PORT=$(id)", 1), strings.Replace(valid, "eceasy/cli-proxy-api", "unapproved/image", 1)} {
		if validateBootstrapEnv(data, p) == nil {
			t.Fatal("unsafe bootstrap env accepted")
		}
	}
}

func TestNewServerBootstrapUsesOnlyBuiltInTemplate(t *testing.T) {
	for _, tampered := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "tampered"}[tampered], func(t *testing.T) {
			restored, _, _ := deploymentFixture(t)
			p, _ := defaultRestorePolicy()
			content, _ := portalops.HostAssets.ReadFile("backup/upstream.approved.yaml")
			if tampered {
				content = []byte("services:\n  dangerous:\n    privileged: true\n")
			}
			if err := os.WriteFile(filepath.Join(restored, "upstream/compose.yaml"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			images := map[string]string{"cpa": "eceasy/cli-proxy-api@sha256:" + strings.Repeat("a", 64), "cpamp": "seakee/cpa-manager-plus@sha256:" + strings.Repeat("b", 64)}
			if err := writeJSON(filepath.Join(restored, "manifest.json"), Manifest{Images: images}); err != nil {
				t.Fatal(err)
			}
			env := "COMPOSE_PROJECT_NAME=cpamp\nCPA_IMAGE=" + images["cpa"] + "\nCPAMP_IMAGE=" + images["cpamp"] + "\nCPA_PORT=8317\nCPAMP_PORT=18317\n"
			if err := os.WriteFile(filepath.Join(restored, "upstream/.env"), []byte(env), 0o600); err != nil {
				t.Fatal(err)
			}
			h, _ := testHostInstaller(t)
			d := dockerPortal{policy: p, installer: &h, execute: func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] != "ps" {
					t.Fatalf("unexpected docker work: %v", args)
				}
				return []byte("cliproxy-portal\n"), nil
			}}
			err := d.bootstrapDeployment(t.Context(), restored)
			if (err == nil) == tampered {
				t.Fatalf("bootstrap result: %v", err)
			}
			got, readErr := os.ReadFile(h.path(p.ComposeFile))
			if tampered {
				if !os.IsNotExist(readErr) {
					t.Fatal("unapproved template installed")
				}
			} else if readErr != nil || !bytes.Equal(got, content) {
				t.Fatal("approved template not installed")
			}
		})
	}
}

func TestPortalRecreateRefreshesSecretMountAndKeepsStableConfig(t *testing.T) {
	h, _ := testHostInstaller(t)
	for _, name := range []string{"compose.prebuilt.yaml", "compose.gateway.yaml"} {
		if err := h.write(defaultPortalRoot+"/"+name, []byte("services: {}\n"), 0o600, false); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := defaultRestorePolicy()
	var commands []string
	files := defaultPortalRoot + "/compose.prebuilt.yaml," + defaultPortalRoot + "/compose.gateway.yaml"
	d := dockerPortal{policy: p, installer: &h, hostESXi: true, execute: func(_ context.Context, args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(args, " "))
		if args[0] == "inspect" {
			return json.Marshal(portalInspection{Labels: map[string]string{"com.docker.compose.project.working_dir": defaultPortalRoot, "com.docker.compose.service": "cliproxy-portal", "com.docker.compose.project": "cliproxy-portal", "com.docker.compose.project.config_files": files}})
		}
		if args[0] != "compose" {
			t.Fatalf("did not recreate portal: %v", args)
		}
		return nil, nil
	}}
	if err := d.recreatePortal(t.Context()); err != nil {
		t.Fatal(err)
	}
	stable := defaultPortalRoot + "/ops/compose.restore-monitoring.json"
	data, err := os.ReadFile(h.path(stable))
	if err != nil || !bytes.Contains(data, []byte("/data/esxi-metrics.json")) {
		t.Fatal("stable monitoring config missing")
	}
	if !strings.Contains(strings.Join(commands, "\n"), "--force-recreate cliproxy-portal") {
		t.Fatal("secret bind mount was not refreshed")
	}
	files += "," + stable
	d.hostESXi = false
	d.portalHostMetrics = ""
	if err := d.recreatePortal(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(h.path(stable))
	if bytes.Contains(data, []byte("/data/esxi-metrics.json")) {
		t.Fatal("rollback retained restored monitor setting")
	}
}
