package agy

import (
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestEmbeddedBridgeRequiresExternalCLI(t *testing.T) {
	t.Setenv("AGY_COMMAND", t.TempDir()+"/missing-agy")
	if close, _, err := StartEmbedded(strings.Repeat("s", 64)); err == nil {
		close()
		t.Fatal("embedded mode started without an external executable")
	}
}

func TestEmbeddedBridgeOwnsOnlyLoopbackAndCloses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux container runtime")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_COMMAND", exe)
	t.Setenv("AGY_HOME", t.TempDir())
	close, done, err := StartEmbedded(strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	// Listener ownership prevents a second in-process bridge from silently
	// binding another endpoint while the portal still uses the first.
	if l, err := net.Listen("tcp", "127.0.0.1:18320"); err == nil {
		l.Close()
		close()
		t.Fatal("embedded endpoint not bound")
	}
	close()
	<-done
	l, err := net.Listen("tcp", "127.0.0.1:18320")
	if err != nil {
		t.Fatal("embedded listener leaked", err)
	}
	l.Close()
}
