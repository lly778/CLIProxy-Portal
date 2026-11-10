package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const agyHome = "/var/lib/cliproxy-agy"
const agySecretPath = "/etc/cliproxy-agy/service.secret"
const agyTokenPath = agyHome + "/agy/antigravity-oauth-token"
const agyConfigPath = "/etc/cliproxy-agy/restore.json"

// Typed data only: no archived programs, units, commands, or conversation data.
type agyCredentials struct {
	Secret         string          `json:"service_secret"`
	OAuth          json.RawMessage `json:"oauth,omitempty"`
	InstallationID string          `json:"installation_id,omitempty"`
	MaxSessions    int             `json:"max_sessions"`
	MemoryMiB      int             `json:"memory_mib"`
}

func (a agyCredentials) validate() error {
	if len(a.Secret) < 32 || len(a.Secret) > 256 || strings.ContainsAny(a.Secret, "\r\n\x00") || a.MaxSessions < 1 || a.MaxSessions > 16 || a.MemoryMiB < 128 || a.MemoryMiB > 4096 || len(a.InstallationID) > 256 || strings.ContainsAny(a.InstallationID, "\r\n\x00") {
		return errors.New("AGY backup data invalid")
	}
	if len(a.OAuth) > 0 {
		var token struct {
			Token json.RawMessage `json:"token"`
		}
		if len(a.OAuth) > 256<<10 || json.Unmarshal(a.OAuth, &token) != nil || len(token.Token) == 0 || string(token.Token) == "null" {
			return errors.New("AGY login data invalid")
		}
	}
	return nil
}

func privateAGYFile(path string, required bool) ([]byte, error) {
	// OpenRoot forbids path traversal through a symlink in any component.
	parent := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return nil, errors.New("AGY 凭据目录不能通过链接重定向")
	}
	if st, err := os.Stat(parent); err != nil || !st.IsDir() || runtime.GOOS == "linux" && st.Mode().Perm()&0022 != 0 {
		return nil, errors.New("AGY 凭据目录权限不安全")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, errors.New("AGY 凭据目录无法读取")
	}
	defer root.Close()
	info, err := root.Lstat(filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS == "linux" && info.Mode().Perm()&0077 != 0 || info.Size() > 256<<10 {
		return nil, errors.New("AGY 凭据缺失、权限不安全或过大")
	}
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, errors.New("AGY 凭据无法读取")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256<<10+1))
	if err != nil || len(data) > 256<<10 {
		return nil, errors.New("AGY 凭据无法读取或过大")
	}
	return data, nil
}

func snapshotAGY(etc, home, portal string) (*agyCredentials, error) {
	dir := filepath.Join(etc, "cliproxy-agy")
	// install-host prepares empty fixed directories even before AGY is
	// configured. Only actual configuration/credential files mark it present.
	configured := false
	for _, file := range []string{filepath.Join(dir, "service.secret"), filepath.Join(dir, "runtime.env"), filepath.Join(portal, "secrets/portal_agy_secret"), filepath.Join(home, "agy/antigravity-oauth-token")} {
		if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
			configured = true
			break
		}
	}
	if !configured {
		return nil, nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(filepath.Join(portal, "secrets/portal_agy_secret")); !errors.Is(e, os.ErrNotExist) {
			return nil, errors.New("AGY 已配置但缺少宿主机凭据，不能生成完整备份")
		}
		return nil, nil
	} else if err != nil {
		return nil, errors.New("AGY 配置无法检查")
	}
	secret, err := privateAGYFile(filepath.Join(dir, "service.secret"), true)
	if err != nil {
		return nil, err
	}
	portalSecret, err := privateAGYFile(filepath.Join(portal, "secrets/portal_agy_secret"), true)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(bytes.TrimSpace(secret), bytes.TrimSpace(portalSecret)) {
		return nil, errors.New("AGY 门户和服务密钥不一致，停止备份")
	}
	a := &agyCredentials{Secret: strings.TrimSpace(string(secret))}
	a.OAuth, err = privateAGYFile(filepath.Join(home, "agy/antigravity-oauth-token"), false)
	if err != nil {
		return nil, err
	}
	id, err := privateAGYFile(filepath.Join(home, "agy/installation_id"), false)
	if err != nil {
		return nil, err
	}
	a.InstallationID = strings.TrimSpace(string(id))
	// Only the combined-container scalar configuration is supported.
	runtimeFile := filepath.Join(dir, "runtime.env")
	if trustedDeploymentFile(runtimeFile) != nil {
		return nil, errors.New("AGY 容器配置缺失或权限无效")
	}
	data, err := os.ReadFile(runtimeFile)
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("AGY 容器配置无法读取")
	}
	seen := make(map[string]bool)
	embedded := false
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || k != "PORTAL_AGY_EMBEDDED" && k != "AGY_MAX_SESSIONS" && k != "AGY_CONTAINER_MEMORY_MIB" {
			continue
		}
		if seen[k] {
			return nil, errors.New("AGY 容器配置含重复项")
		}
		seen[k] = true
		if k == "PORTAL_AGY_EMBEDDED" {
			embedded = v == "true"
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, errors.New("AGY 容器资源配置无效")
		}
		if k == "AGY_MAX_SESSIONS" {
			a.MaxSessions = n
		} else {
			a.MemoryMiB = n
		}
	}
	if !embedded || a.validate() != nil {
		return nil, errors.New("AGY 容器凭据或资源配置无效")
	}
	return a, nil
}
