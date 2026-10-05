// Package backup implements opt-in, encrypted disaster backups.
// The host runner has access to an operator-owned sources file; the web process
// can manage repository credentials and enqueue jobs, but cannot select host files.
package backup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Enabled    bool   `json:"enabled"`
	Repository string `json:"repository"`
	Hour       int    `json:"hour"`
	Minute     int    `json:"minute,omitempty"`
	Weekday    int    `json:"weekday"` // 0=Sunday, 1=Monday, ..., 6=Saturday.
	Retain     int    `json:"retain"`
	KeySaved   bool   `json:"key_saved"`
	Instance   string `json:"instance"`
}

type Status struct {
	Repository        string    `json:"repository"`
	SuccessRepository string    `json:"success_repository"`
	Running           bool      `json:"running"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	LastSuccessAt     time.Time `json:"last_success_at"`
	LastAttemptDay    string    `json:"last_attempt_day"`
	Message           string    `json:"message"`
	ReleaseURL        string    `json:"release_url"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]{1,100}$`)
var instancePattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

func Validate(c Config) error {
	if !repositoryPattern.MatchString(c.Repository) || strings.HasSuffix(c.Repository, ".git") || c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 || c.Weekday < 0 || c.Weekday > 6 || c.Retain < 1 || c.Retain > 30 || !instancePattern.MatchString(c.Instance) {
		return errors.New("仓库请填写 owner/repo；星期应为 0–6（0 为周日），时间应为 00:00–23:59，保留数量应为 1–30 份")
	}
	if !c.KeySaved {
		return errors.New("请先下载并另行保存恢复密钥")
	}
	return nil
}

func LoadConfig(dir string) (Config, error) {
	c := Config{Hour: 3, Weekday: 1, Retain: 3}
	err := readJSON(filepath.Join(dir, "config.json"), &c)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	return c, err
}

// SaveConfig never generates a recovery key: UI setup is deliberate and opt-in.
func SaveConfig(dir string, c Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "config.json"), c)
}

func LoadStatus(dir string) (Status, error) {
	var s Status
	err := readJSON(filepath.Join(dir, "status.json"), &s)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	return s, err
}

func NewInstance() (string, error) {
	b := make([]byte, 8)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

func EnsureKey(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "recovery.key")
	key, err := ReadKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return ReadKey(path)
	}
	if err != nil {
		return nil, err
	}
	if err = inheritOwner(dir, f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	_, err = f.WriteString(hex.EncodeToString(key) + "\n")
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return key, nil
}

func ReadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != 32 {
		return nil, errors.New("恢复密钥格式无效")
	}
	return key, nil
}

func LoadToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "github.token"))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if err := ValidateToken(token); err != nil {
		return "", err
	}
	return token, nil
}

func ValidateToken(token string) error {
	if len(token) < 20 || len(token) > 512 || strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r >= 127 }) >= 0 {
		return errors.New("GitHub 令牌格式无效")
	}
	return nil
}

func SaveToken(dir, token string) error {
	if err := ValidateToken(token); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "github.token"), []byte(token+"\n"), 0o600)
}

func Request(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "pending"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return errors.New("已有备份请求等待执行")
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func Pending(dir string) bool { _, err := os.Stat(filepath.Join(dir, "pending")); return err == nil }

func RunnerSeen(dir string) time.Time {
	var t time.Time
	_ = readJSON(filepath.Join(dir, "heartbeat.json"), &t)
	return t
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0o644)
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = inheritOwner(filepath.Dir(path), f); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save backup state: %w", err)
	}
	return nil
}
