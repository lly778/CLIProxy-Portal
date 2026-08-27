package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr          string
	DatabasePath        string
	ExternalURL         string
	CPAAPIBaseURL       string
	CPAMPBaseURL        string
	CPAMPAdminKeyFile   string
	AppSecretFile       string
	CookieName          string
	TimeZone            *time.Location
	ReconcileInterval   time.Duration
	PendingRetry        time.Duration
	CPAMPTimeout        time.Duration
	UsageCacheTTL       time.Duration
	TrustProxyHeaders   bool
	RegistrationDefault bool
}

func Load() (Config, error) {
	loc, err := time.LoadLocation(env("PORTAL_TIME_ZONE", "Asia/Shanghai"))
	if err != nil {
		return Config{}, fmt.Errorf("load time zone: %w", err)
	}
	cfg := Config{
		ListenAddr:          env("PORTAL_LISTEN_ADDR", ":18080"),
		DatabasePath:        env("PORTAL_DATABASE_PATH", "/data/portal.db"),
		ExternalURL:         strings.TrimRight(env("PORTAL_EXTERNAL_URL", "http://localhost:18080"), "/"),
		CPAAPIBaseURL:       strings.TrimRight(env("CPA_API_BASE_URL", "http://localhost:8317"), "/"),
		CPAMPBaseURL:        strings.TrimRight(env("CPAMP_BASE_URL", "http://cpa-manager-plus:18317"), "/"),
		CPAMPAdminKeyFile:   env("CPAMP_ADMIN_KEY_FILE", "/run/secrets/cpamp_admin_key"),
		AppSecretFile:       env("PORTAL_APP_SECRET_FILE", "/run/secrets/portal_app_secret"),
		CookieName:          env("PORTAL_COOKIE_NAME", "cliproxy_portal_session"),
		TimeZone:            loc,
		ReconcileInterval:   durationEnv("PORTAL_RECONCILE_INTERVAL", 5*time.Minute),
		PendingRetry:        durationEnv("PORTAL_PENDING_RETRY", 30*time.Second),
		CPAMPTimeout:        durationEnv("CPAMP_TIMEOUT", 15*time.Second),
		UsageCacheTTL:       durationEnv("PORTAL_USAGE_CACHE_TTL", 2*time.Minute),
		TrustProxyHeaders:   boolEnv("PORTAL_TRUST_PROXY_HEADERS", false),
		RegistrationDefault: boolEnv("PORTAL_REGISTRATION_OPEN", true),
	}
	if cfg.CPAMPBaseURL == "" || cfg.CPAAPIBaseURL == "" {
		return Config{}, errors.New("CPAMP_BASE_URL and CPA_API_BASE_URL are required")
	}
	return cfg, nil
}

func ReadSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) < 32 {
		return nil, fmt.Errorf("secret in %s must be at least 32 characters", path)
	}
	return b, nil
}

func ReadTextSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("secret in %s must not be empty", path)
	}
	return v, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func boolEnv(name string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
