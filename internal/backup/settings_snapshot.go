package backup

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const backupSettingsName = "portal/backup/settings.json"

// Portable preferences exclude secrets (stored in the separate encrypted
// credential snapshot) and the installation identity used to scope retention.
type portableBackupSettings struct {
	Version    int    `json:"version"`
	Enabled    bool   `json:"enabled"`
	Repository string `json:"repository"`
	Hour       int    `json:"hour"`
	Minute     int    `json:"minute"`
	Weekday    int    `json:"weekday"`
	Retain     int    `json:"retain"`
}

func snapshotBackupSettings(c Config, work string) (Source, error) {
	v := portableBackupSettings{1, c.Enabled, c.Repository, c.Hour, c.Minute, c.Weekday, c.Retain}
	file := filepath.Join(work, "backup-settings.json")
	if err := writeJSON(file, v); err != nil {
		return Source{}, err
	}
	return Source{Path: file, Name: backupSettingsName, Required: true}, nil
}

func readBackupSettings(file string) (portableBackupSettings, error) {
	var v portableBackupSettings
	info, err := os.Lstat(file)
	if err != nil {
		return v, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return v, errors.New("备份设置必须是受限大小的普通文件")
	}
	f, err := os.Open(file)
	if err != nil {
		return v, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 4097))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&v); err != nil {
		return v, errors.New("备份设置格式无效")
	}
	if decoder.Decode(new(any)) != io.EOF || v.Version != 1 {
		return v, errors.New("备份设置版本或格式无效")
	}
	check := Config{Repository: v.Repository, Hour: v.Hour, Minute: v.Minute, Weekday: v.Weekday, Retain: v.Retain, KeySaved: true, Instance: "0000000000000000"}
	if err = Validate(check); err != nil {
		return v, err
	}
	return v, nil
}

func prepareBackupSettings(dir, candidate string, restoredCredentials bool) error {
	v, err := readBackupSettings(candidate)
	if err != nil {
		return err
	}
	c, err := LoadConfig(dir)
	if err != nil {
		return errors.New("当前备份设置无法读取")
	}
	if c.Instance == "" {
		c.Instance, err = NewInstance()
		if err != nil {
			return err
		}
	} else if !instancePattern.MatchString(c.Instance) {
		return errors.New("当前备份安装标识无效")
	}
	_, keyErr := ReadKey(filepath.Join(dir, "recovery.key"))
	_, tokenErr := LoadToken(dir)
	ready := c.Repository == v.Repository && c.KeySaved && keyErr == nil && tokenErr == nil
	if restoredCredentials {
		ready = true
		c.KeySaved = true
	}
	c.Enabled = v.Enabled && ready
	c.Repository, c.Hour, c.Minute, c.Weekday, c.Retain = v.Repository, v.Hour, v.Minute, v.Weekday, v.Retain
	if !restoredCredentials {
		c.KeySaved = c.KeySaved && keyErr == nil
	}
	// Fresh installations can import preferences before separately setting their
	// credentials. Scheduling remains disabled until the operator confirms them.
	return writeJSON(candidate, c)
}

func backupSettingsTarget(dir string) restoreTarget {
	return restoreTarget{Path: filepath.Join(dir, "config.json"), Archive: backupSettingsName, Kind: "backup-settings"}
}

func restoreTargets(p PortalRestorePolicy, dir, mode string) ([]restoreTarget, error) {
	targets := p.targets(mode)
	target := backupSettingsTarget(dir)
	if !filepath.IsAbs(target.Path) || filepath.Clean(target.Path) != target.Path {
		return nil, errors.New("备份设置目标目录无效")
	}
	for _, previous := range targets {
		if target.Path == previous.Path || (previous.Kind == "directory" && within(target.Path, previous.Path)) {
			return nil, errors.New("备份设置目标与恢复文件重叠")
		}
	}
	return append(targets, target), nil
}
