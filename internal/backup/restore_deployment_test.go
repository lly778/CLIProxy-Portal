package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func deploymentFixture(t *testing.T) (string, PortalRestorePolicy, map[string]string) {
	t.Helper()
	_, restored, p := restoreFixture(t)
	compose := []byte("services:\n  cpa:\n    image: ${CPA_IMAGE}\n  cpamp:\n    image: ${CPAMP_IMAGE}\n")
	for _, file := range []string{p.ComposeFile, filepath.Join(restored, "upstream/compose.yaml")} {
		if err := os.WriteFile(file, compose, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := sha256.Sum256(compose)
	p.ComposeSHA256 = hex.EncodeToString(digest[:])
	key, _ := os.ReadFile(filepath.Join(restored, "upstream/secrets/cpa-management-key"))
	hash, err := bcrypt.GenerateFromPassword(key, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	config := []byte("port: 8317\nauth-dir: /root/.cli-proxy-api\napi-keys: [backup-key]\nremote-management:\n  secret-key: '" + string(hash) + "'\n")
	if err = os.WriteFile(filepath.Join(restored, "upstream/cliproxyapi/config.yaml"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	images := map[string]string{"cpa": "cpa@sha256:" + strings.Repeat("a", 64), "cpamp": "cpamp@sha256:" + strings.Repeat("b", 64)}
	if err = writeJSON(filepath.Join(restored, "manifest.json"), Manifest{Images: images}); err != nil {
		t.Fatal(err)
	}
	return restored, p, images
}

func TestDatabaseDockerDriverNeverOperatesOnUpstreams(t *testing.T) {
	_, _, p := restoreFixture(t)
	running := true
	var commands []string
	d := &dockerPortal{policy: p, execute: func(_ context.Context, args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(args, " "))
		switch args[0] {
		case "ps":
			return []byte("portal\ncpa\nmanager\n"), nil
		case "stop":
			if args[len(args)-1] != p.Container {
				t.Fatal("stopped upstream")
			}
			running = false
			return nil, nil
		case "start":
			if args[len(args)-1] != p.Container {
				t.Fatal("started upstream")
			}
			return nil, nil
		case "inspect":
			if args[len(args)-1] != p.Container {
				t.Fatal("inspected upstream")
			}
			return json.Marshal(map[string]any{"State": map[string]any{"Running": running, "Health": map[string]string{"Status": "healthy"}}, "Mounts": []map[string]any{{"Source": filepath.Dir(p.Database), "Destination": "/data", "RW": true}}})
		default:
			t.Fatalf("unexpected Docker operation %s", args[0])
			return nil, nil
		}
	}}
	if err := d.Verify(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(commands) == 0 {
		t.Fatal("no commands")
	}
}

func TestRebuildRequiresApprovedStructureAndPins(t *testing.T) {
	for _, scenario := range []string{"valid", "image", "missing-pins", "profile", "template", "ports", "environment", "key", "cpa-key", "config", "pull-failure"} {
		t.Run(scenario, func(t *testing.T) {
			restored, p, images := deploymentFixture(t)
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(restored, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "image":
				images["cpa"] = "unapproved/image@sha256:" + strings.Repeat("a", 64)
			case "missing-pins":
				images = map[string]string{}
			case "profile":
				p.ComposeSHA256 = strings.Repeat("0", 64)
			case "template":
				write("upstream/compose.yaml", "services: {}")
			case "key":
				write("portal/secrets/cpamp_admin_key", "mismatched-manager-key")
			case "cpa-key":
				write("upstream/secrets/cpa-management-key", "mismatched-cpa-management-key")
			case "config":
				write("upstream/cliproxyapi/config.yaml", "port: 9999")
			}
			if err := writeJSON(filepath.Join(restored, "manifest.json"), Manifest{Images: images}); err != nil {
				t.Fatal(err)
			}
			pulls, ups := 0, 0
			d := &dockerPortal{policy: p, rebuild: true, execute: func(_ context.Context, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if args[0] == "start" {
					return nil, nil
				}
				if args[0] == "ps" {
					return []byte(p.ManagerContainer), nil
				}
				if args[0] == "volume" {
					if args[1] == "ls" {
						return []byte(p.ManagerVolume), nil
					}
					if args[1] == "inspect" {
						return []byte(filepath.Dir(p.ManagerDatabase)), nil
					}
					t.Fatal("unexpected volume mutation")
				}
				if args[0] != "compose" {
					t.Fatalf("unsafe operation before validation: %s", args[0])
				}
				if strings.Contains(joined, " config --format json") {
					port := "8317"
					env := "approved"
					if strings.Contains(joined, filepath.Join(restored, "upstream/.env")) {
						if scenario == "ports" {
							port = "9999"
						}
						if scenario == "environment" {
							env = "different"
						}
					}
					return json.Marshal(map[string]any{
						"services": map[string]any{
							"cpa":            map[string]any{"ports": []string{port}, "environment": map[string]string{"FIXED": env}},
							p.ManagerService: map[string]any{"volumes": []map[string]any{{"type": "volume", "source": "data", "target": "/data"}}, "environment": map[string]string{"USAGE_DB_PATH": "/data/usage.sqlite", "CPA_MANAGER_DATA_KEY_PATH": "/data/data.key"}},
						}, "volumes": map[string]any{"data": map[string]string{"name": p.ManagerVolume}},
					})
				}
				// The private override is readable only during the command and pins both services.
				var override string
				for i, arg := range args {
					if arg == "--file" && args[i+1] != p.ComposeFile {
						override = args[i+1]
					}
				}
				data, err := os.ReadFile(override)
				if err != nil {
					t.Fatal(err)
				}
				for _, image := range images {
					if !bytes.Contains(data, []byte(image)) {
						t.Fatal("image not pinned")
					}
				}
				if strings.Contains(joined, " pull ") {
					pulls++
					if scenario == "pull-failure" {
						return nil, errors.New("registry failed")
					}
					return nil, nil
				}
				if strings.Contains(joined, " up --detach --no-deps --force-recreate ") {
					ups++
					return nil, nil
				}
				t.Fatalf("unexpected compose operation %s", joined)
				return nil, nil
			}}
			err := d.Prepare(t.Context(), restored)
			if scenario != "valid" {
				if err == nil {
					t.Fatal("unsafe rebuild accepted")
				}
				if ups != 0 || (scenario != "pull-failure" && pulls != 0) {
					t.Fatal("deployment reached before validation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if pulls != 1 || ups != 0 {
				t.Fatal("must download before stopping services")
			}
			if err = d.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if ups != 1 {
				t.Fatal("no rebuild")
			}
		})
	}
}

func TestMissingUpstreamRollbackRemovesOnlyNewContainers(t *testing.T) {
	_, p, images := deploymentFixture(t)
	d := &dockerPortal{policy: p, rebuild: true, images: images}
	d.execute = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] != "ps" {
			t.Fatal("unexpected command")
		}
		return []byte("portal"), nil
	}
	state, err := d.OriginalDeployment(t.Context())
	if err != nil || !state.Missing {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if err = d.RestoreDeployment(state); err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	d.execute = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "ps":
			return []byte("portal\ncpa\nmanager\nunrelated"), nil
		case "rm":
			if len(args) != 2 || (args[1] != p.UpstreamContainer && args[1] != p.ManagerContainer) {
				t.Fatal("unsafe removal")
			}
			removed[args[1]] = true
		case "start":
			if args[1] != p.Container {
				t.Fatal("unsafe start")
			}
		default:
			t.Fatal("unexpected Docker operation")
		}
		return nil, nil
	}
	if err = d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatal("new containers not rolled back")
	}
}

func TestCaptureRecordsRepositoryDigestsNotMutableTags(t *testing.T) {
	_, p, images := deploymentFixture(t)
	got, err := captureImagesWith(t.Context(), p, func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "inspect" {
			return []byte(args[len(args)-1]), nil
		}
		if args[0] == "image" {
			role := "cpa"
			if args[len(args)-1] == p.ManagerContainer {
				role = "cpamp"
			}
			return json.Marshal([]string{images[role]})
		}
		t.Fatal("unexpected operation")
		return nil, nil
	})
	if err != nil || got["cpa"] != images["cpa"] || got["cpamp"] != images["cpamp"] {
		t.Fatalf("digests=%v err=%v", got, err)
	}
}

func TestManagementHealthRequiresBothRestoredKeysAndManagerProxy(t *testing.T) {
	_, p, _ := deploymentFixture(t)
	cpaKey, _ := os.ReadFile(p.CPAKeyFile)
	managerKey, _ := os.ReadFile(p.ManagerKeyFile)
	denied := false
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.URL.Path != "/v0/management/config.yaml" {
			t.Error("unexpected management route")
			w.WriteHeader(404)
			return
		}
		auth := r.Header.Get("Authorization")
		if denied || (auth != "Bearer "+string(cpaKey) && auth != "Bearer "+string(managerKey)) {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	p.UpstreamHealthURL = server.URL + "/"
	p.ManagerHealthURL = server.URL + "/health"
	d := &dockerPortal{policy: p, rebuild: true}
	if !d.managementReady(t.Context(), server.Client()) || len(calls) != 2 {
		t.Fatal("both authenticated routes must pass")
	}
	denied = true
	if d.managementReady(t.Context(), server.Client()) {
		t.Fatal("invalid management connection treated as healthy")
	}
}
