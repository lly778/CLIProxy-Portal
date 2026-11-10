package agy

import (
	"bufio"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAGYAuthHelperProcess(t *testing.T) {
	if os.Getenv("AGY_AUTH_HELPER") != "1" {
		return
	}
	fmt.Println("https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=test")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.HasPrefix(line, "bad-") {
		os.Exit(2)
	}
	home := os.Getenv("AGY_AUTH_HELPER_HOME")
	_ = os.MkdirAll(filepath.Join(home, "agy"), 0700)
	_ = os.WriteFile(accountPath(home), []byte(`{"id_token_email":"new@example.com","token":{"access_token":"unit-test-only"}}`), 0600)
	os.Exit(0)
}
func authTestManager(t *testing.T) *Manager {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(home, "agy"), 0700)
	_ = os.WriteFile(accountPath(home), []byte(`{"id_token_email":"old@example.com","token":{"access_token":"unit-test-only"}}`), 0600)
	life, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: RuntimeConfig{Home: home}, life: life, slot: make(chan struct{}, 1), sessions: map[string]*session{}, authCommandTest: func(ctx context.Context, stage string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestAGYAuthHelperProcess$")
		cmd.Env = append(os.Environ(), "AGY_AUTH_HELPER=1", "AGY_AUTH_HELPER_HOME="+stage)
		return cmd
	}}
	return m
}
func TestAGYLoginCommitsOnlyVerifiedOwnerAndInvalidatesOldSessionRoutes(t *testing.T) {
	m := authTestManager(t)
	d, err := openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	v := diskTestRecord("old-native", "user:key", time.Now())
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	ch, err := m.startLogin(t.Context(), "admin:a")
	if err != nil {
		t.Fatal(err)
	}
	if readAccount(m.cfg.Home).Email != "old@example.com" {
		t.Fatal("starting login replaced the current account")
	}
	if _, err = m.finishLogin(t.Context(), "admin:b", ch.ID, "valid-auth-code"); err == nil {
		t.Fatal("another administrator submitted this login")
	}
	a, err := m.finishLogin(t.Context(), "admin:a", ch.ID, "valid-auth-code")
	if err != nil || a.Email != "new@example.com" || readAccount(m.cfg.Home).Email != a.Email {
		t.Fatalf("account switch failed: %+v %v", a, err)
	}
	if s, e := d.find(v.Owner, request{Previous: "resp-old-native"}, nil); e != nil || s != nil {
		t.Fatal("old account response chain remained recoverable")
	}
	if err = m.logout(t.Context()); err != nil || readAccount(m.cfg.Home).LoggedIn {
		t.Fatal("logout left the account usable")
	}
}
func TestAGYRejectedAuthorizationPreservesAccount(t *testing.T) {
	m := authTestManager(t)
	ch, err := m.startLogin(t.Context(), "admin:a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.finishLogin(t.Context(), "admin:a", ch.ID, "bad-auth-code"); err == nil {
		t.Fatal("failed CLI authorization was committed")
	}
	if readAccount(m.cfg.Home).Email != "old@example.com" {
		t.Fatal("failed authorization replaced the original account")
	}
	for _, code := range []string{"a\nbbbbbbbb", "a\x00bbbbbbbb", "https://example.com/?code=123456789"} {
		if _, err := loginCode(code); err == nil {
			t.Fatal("invalid auth input accepted")
		}
	}
	if u := googleAuthURL("https://accounts.google.com.evil.example/o/oauth2/auth?response_type=code"); u != "" {
		t.Fatal("untrusted authorization host accepted")
	}
	r := httptest.NewRequest("POST", "/internal/account/login", nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 64))
	m.cfg.Secret = strings.Repeat("s", 64)
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("account operation accepted missing admin identity")
	}
}
func TestAGYCancelLoginKeepsCurrentAccount(t *testing.T) {
	m := authTestManager(t)
	ch, err := m.startLogin(t.Context(), "admin:a")
	if err != nil {
		t.Fatal(err)
	}
	m.cancelLogin("admin:b", ch.ID)
	if m.authJob == nil {
		t.Fatal("another administrator cancelled the login")
	}
	m.cancelLogin("admin:a", ch.ID)
	if m.authJob != nil || readAccount(m.cfg.Home).Email != "old@example.com" {
		t.Fatal("cancel changed the signed-in account")
	}
}
