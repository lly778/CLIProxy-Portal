package agy

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func RunService() error {
	secretFile := env("PORTAL_AGY_SECRET_FILE", "/run/secrets/portal_agy_secret")
	b, e := os.ReadFile(secretFile)
	if e != nil {
		return errors.New("AGY service secret cannot be read")
	}
	limit, _ := strconv.Atoi(env("AGY_MAX_SESSIONS", "3"))
	m, e := NewManager(RuntimeConfig{Command: env("AGY_COMMAND", "/usr/local/bin/agy"), Home: env("AGY_HOME", "/data"), Secret: strings.TrimSpace(string(b)), MaxSessions: limit})
	if e != nil {
		return e
	}
	defer m.Close()
	s := &http.Server{Addr: env("PORTAL_AGY_LISTEN_ADDR", ":18320"), Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10, IdleTimeout: 90 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- s.ListenAndServe() }()
	select {
	case e := <-errs:
		if !errors.Is(e, http.ErrServerClosed) {
			return e
		}
	case <-ctx.Done():
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		return s.Shutdown(c)
	}
	return nil
}
func env(k, v string) string {
	if s := strings.TrimSpace(os.Getenv(k)); s != "" {
		return s
	}
	return v
}
