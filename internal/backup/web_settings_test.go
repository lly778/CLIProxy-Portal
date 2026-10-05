package backup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWebSettingsPreserveScheduleIdentityAndRecoveryKey(t *testing.T) {
	dir := t.TempDir()
	key, err := EnsureKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := Config{Repository: "owner/private", Instance: "0123456789abcdef", Weekday: 6, Hour: 21, Retain: 7, Enabled: false, KeySaved: true}
	if err = SaveConfig(dir, old); err != nil {
		t.Fatal(err)
	}
	if err = SaveToken(dir, "test-only-old-secret-token"); err != nil {
		t.Fatal(err)
	}
	f, g := fakeClient(t)
	replacement := "test-only-new-secret-token"
	client := func(token string) *GitHub {
		if token != replacement {
			t.Fatal("replacement token not checked")
		}
		return g
	}
	if err = WithIdleState(dir, func() error {
		return configureSettings(t.Context(), dir, "owner/private", replacement, true, false, 6, 21, 0, client)
	}); err != nil {
		t.Fatal(err)
	}
	saved, _ := LoadConfig(dir)
	if saved != old {
		t.Fatalf("settings changed: %+v", saved)
	}
	after, _ := ReadKey(filepath.Join(dir, "recovery.key"))
	if !bytes.Equal(key, after) {
		t.Fatal("key rotated")
	}
	if err = configureSettings(t.Context(), dir, "owner/private", "", true, false, 6, 21, 0, func(string) *GitHub { t.Fatal("unchanged paused config accessed GitHub"); return nil }); err != nil {
		t.Fatal(err)
	}
	f.private = false
	if err = configureSettings(t.Context(), dir, "owner/private", "test-only-rejected-token", true, false, 6, 21, 0, func(string) *GitHub { return g }); err == nil {
		t.Fatal("public repository accepted")
	}
	token, _ := LoadToken(dir)
	if token != replacement {
		t.Fatal("rejected validation changed token")
	}
	if err = configureSettings(t.Context(), dir, "owner/private", replacement, false, false, 6, 21, 0, client); err == nil {
		t.Fatal("missing key-saved confirmation accepted")
	}
	f.private = true
	if err = configureSettings(t.Context(), dir, "owner/private", "", true, true, 3, 8, 35, client); err != nil {
		t.Fatal(err)
	}
	saved, _ = LoadConfig(dir)
	if !saved.Enabled || saved.Weekday != 3 || saved.Hour != 8 || saved.Minute != 35 || saved.Instance != old.Instance || saved.Retain != old.Retain {
		t.Fatalf("schedule not saved safely: %+v", saved)
	}
	for _, plan := range [][3]int{{-1, 3, 0}, {7, 3, 0}, {1, -1, 0}, {1, 24, 0}, {1, 3, -1}, {1, 3, 60}} {
		if err = configureSettings(t.Context(), dir, "owner/private", "", true, true, plan[0], plan[1], plan[2], client); err == nil {
			t.Fatal("invalid schedule accepted")
		}
	}
	afterPlan, _ := LoadConfig(dir)
	if afterPlan != saved {
		t.Fatal("invalid schedule changed settings")
	}
}

func TestWebSettingsFirstSetupAndSharedIdleLock(t *testing.T) {
	dir := t.TempDir()
	_, g := fakeClient(t)
	client := func(string) *GitHub { return g }
	if err := configureSettings(t.Context(), dir, "owner/private", "test-only-new-secret-token", true, true, 1, 3, 0, client); err == nil {
		t.Fatal("settings saved without recovery key")
	}
	if _, err := EnsureKey(dir); err != nil {
		t.Fatal(err)
	}
	if err := WithIdleState(dir, func() error {
		return configureSettings(t.Context(), dir, "owner/private", "test-only-new-secret-token", true, true, 1, 3, 0, client)
	}); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadConfig(dir)
	if !c.Enabled || c.Weekday != 1 || c.Hour != 3 || c.Minute != 0 || c.Retain != 3 || c.Instance == "" {
		t.Fatalf("initial defaults: %+v", c)
	}
	called := false
	if err := os.WriteFile(filepath.Join(dir, "runner.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := WithIdleState(dir, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("active runner was not excluded")
	}
	if err := os.Remove(filepath.Join(dir, "runner.lock")); err != nil {
		t.Fatal(err)
	}
	if err := Request(dir); err != nil {
		t.Fatal(err)
	}
	if err := WithIdleState(dir, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("pending request was not excluded")
	}
	if _, err := os.Stat(filepath.Join(dir, "runner.lock")); !os.IsNotExist(err) {
		t.Fatal("failed settings retained its lock")
	}
	if err := os.Remove(filepath.Join(dir, "pending")); err != nil {
		t.Fatal(err)
	}
	if err := WithIdleState(dir, func() error { return errors.New("test failure") }); err == nil {
		t.Fatal("error lost")
	}
	if _, err := os.Stat(filepath.Join(dir, "runner.lock")); !os.IsNotExist(err) {
		t.Fatal("error retained lock")
	}
}
