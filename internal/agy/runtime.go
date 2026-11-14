package agy

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const embeddedAddress = "127.0.0.1:18320"

// StartEmbedded runs the bridge in the portal process. The official CLI stays
// an external, read-only mounted executable; its data lives on a separate bind.
func StartEmbedded(secret string, observers ...func(Observation) error) (func(), <-chan error, error) {
	command := env("AGY_COMMAND", "/opt/cliproxy-agy/bin/agy")
	info, err := os.Stat(command)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, nil, errors.New("AGY official CLI must be installed externally and mounted executable")
	}
	limit, err := strconv.Atoi(env("AGY_MAX_SESSIONS", "3"))
	if err != nil || limit < 1 || limit > 16 {
		return nil, nil, errors.New("invalid AGY session limit")
	}
	l, err := net.Listen("tcp", embeddedAddress)
	if err != nil {
		return nil, nil, err
	}
	var observe func(Observation) error
	experimentalRefresh, err := strconv.ParseBool(env("AGY_CATALOG_REFRESH_EXPERIMENTAL", "false"))
	if err != nil {
		_ = l.Close()
		return nil, nil, errors.New("invalid AGY experimental catalog refresh setting")
	}
	if len(observers) > 0 {
		observe = observers[0]
	}
	m, err := NewManager(RuntimeConfig{ExperimentalCatalogRefresh: experimentalRefresh, Observe: observe, Command: command, Home: env("AGY_HOME", "/var/lib/cliproxy-agy"), Secret: secret, MaxSessions: limit})
	if err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	s := &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10, IdleTimeout: 90 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- s.Serve(l) }()
	return func() {
		_ = s.Close()
		m.Close()
	}, errs, nil
}

func env(k, v string) string {
	if s := strings.TrimSpace(os.Getenv(k)); s != "" {
		return s
	}
	return v
}
