package backup

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Configure reads operator credentials from a file, never CLI args.
func Configure(ctx context.Context, dir string, c Config, tokenFile string) error {
	return WithIdleState(dir, func() error { return configure(ctx, dir, c, tokenFile, NewGitHub) })
}

func configure(ctx context.Context, dir string, c Config, tokenFile string, client func(string) *GitHub) error {
	var replacement *string
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return errors.New("GitHub 令牌文件无法读取")
		}
		value := string(b)
		replacement = &value
	}
	return configureToken(ctx, dir, c, replacement, client)
}

// ConfigureSettings changes repository and schedule, preserving installation
// identity and the configured retention count.
// Authentication, request limits and the idle-state lock belong to the caller.
func ConfigureSettings(ctx context.Context, dir, repository, token string, keySaved, enabled bool, weekday, hour, minute int) error {
	return configureSettings(ctx, dir, repository, token, keySaved, enabled, weekday, hour, minute, NewGitHub)
}

func configureSettings(ctx context.Context, dir, repository, token string, keySaved, enabled bool, weekday, hour, minute int, client func(string) *GitHub) error {
	c, err := LoadConfig(dir)
	if err != nil {
		return errors.New("备份设置无法读取")
	}
	c.Repository, c.KeySaved = strings.TrimSpace(repository), keySaved
	c.Enabled, c.Weekday, c.Hour, c.Minute = enabled, weekday, hour, minute
	var replacement *string
	if value := strings.TrimSpace(token); value != "" {
		replacement = &value
	}
	return configureToken(ctx, dir, c, replacement, client)
}

func configureToken(ctx context.Context, dir string, c Config, replacement *string, client func(string) *GitHub) error {
	old, err := LoadConfig(dir)
	if err != nil {
		return errors.New("备份设置无法读取")
	}
	if c.Instance == "" {
		c.Instance = old.Instance
	}
	if c.Instance == "" {
		c.Instance, err = NewInstance()
		if err != nil {
			return err
		}
	}
	if err = Validate(c); err != nil {
		return err
	}
	if _, err = ReadKey(filepath.Join(dir, "recovery.key")); err != nil {
		return errors.New("请先导出并另行保管恢复密钥")
	}
	var token string
	if replacement == nil {
		token, err = LoadToken(dir)
	} else {
		token = *replacement
	}
	if err != nil {
		return errors.New("GitHub 令牌文件无法读取")
	}
	// SaveToken validates and trims the token at the operator boundary too.
	token = strings.TrimSpace(token)
	if err = ValidateToken(token); err != nil {
		return err
	}
	if c.Enabled || replacement != nil || c.Repository != old.Repository {
		if err = client(token).CheckPrivate(ctx, c.Repository); err != nil {
			return err
		}
	}
	var previous []byte
	var previousErr error
	if replacement != nil {
		previous, previousErr = os.ReadFile(filepath.Join(dir, "github.token"))
		if previousErr != nil && !os.IsNotExist(previousErr) {
			return errors.New("原有 GitHub 令牌无法读取")
		}
		if err = SaveToken(dir, token); err != nil {
			return err
		}
	}
	if err = SaveConfig(dir, c); err != nil && replacement != nil {
		var rollbackErr error
		if previousErr == nil {
			rollbackErr = atomicWrite(filepath.Join(dir, "github.token"), previous, 0o600)
		} else {
			rollbackErr = os.Remove(filepath.Join(dir, "github.token"))
		}
		if rollbackErr != nil {
			return errors.New("备份设置保存失败，令牌回退失败，请检查后台文件权限")
		}
	}
	return err
}

// WithIdleState shares the host runner's exclusive lock. Credential updates
// cannot interleave with snapshots, restores or an already queued request.
func WithIdleState(dir string, fn func() error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock := filepath.Join(dir, "runner.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return errors.New("已有备份或恢复任务正在执行")
	}
	if err != nil {
		return err
	}
	_ = f.Close()
	defer os.Remove(lock)
	b, e1 := LoadStatus(dir)
	r, e2 := LoadRestoreStatus(dir)
	if e1 != nil || e2 != nil || b.Running || r.Running || Pending(dir) || RestorePending(dir) || DatabaseRecoveryPending(dir) || ManagerBackupPending(dir) {
		return errors.New("已有备份或恢复任务正在执行或等待执行")
	}
	return fn()
}

func ExportKey(dir, output string) error {
	a, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	b, err := filepath.Abs(output)
	if err != nil || within(b, a) {
		return errors.New("请将恢复密钥导出到备份状态目录之外")
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("恢复密钥导出文件已存在或目录不可写；不会覆盖")
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(output)
		}
	}()
	key, err := EnsureKey(dir)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
