package backup

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type restoredCredentials struct {
	GitHub bool `json:"github,omitempty"`
	ESXi   bool `json:"esxi,omitempty"`
	Tunnel bool `json:"tunnel,omitempty"`
	Key    bool `json:"key,omitempty"`
}

type credentialRestoreController interface {
	prepareCredentials(string, string, PortalRestorePolicy) (restoredCredentials, error)
}

type recoveryKeyRestoreController interface {
	prepareRecoveryKey(string, string) (bool, error)
}

func (d *dockerPortal) prepareRecoveryKey(restored, dir string) (bool, error) {
	return prepareMissingRecoveryKey(restored, dir, d.recoveryKeyFile)
}

// A database-only restore keeps the installation's existing key. On a fresh
// installation, retain the key that actually decrypted this backup instead of
// leaving imported backup settings without a downloadable recovery key.
// Stage it as a regular transaction target so failure or interruption rolls
// it back together with the database and settings, before the job is removed.
func prepareMissingRecoveryKey(restored, dir, keyFile string) (bool, error) {
	target := filepath.Join(dir, "recovery.key")
	info, err := os.Lstat(target)
	if err == nil {
		if !info.Mode().IsRegular() {
			return false, errors.New("当前恢复密钥必须是普通文件")
		}
		if _, err := ReadKey(target); err != nil {
			return false, errors.New("当前恢复密钥无法读取或格式无效，不能自动更换")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("当前恢复密钥无法检查")
	}
	key, err := ReadKey(keyFile)
	if err != nil {
		return false, errors.New("无法保留本次恢复使用的备份密钥")
	}
	if err := atomicWrite(filepath.Join(restored, "portal/backup/runtime/recovery.key"), []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return false, errors.New("恢复密钥无法暂存")
	}
	return true, nil
}

func (d *dockerPortal) prepareCredentials(restored, dir string, p PortalRestorePolicy) (restoredCredentials, error) {
	flags, err := prepareCredentialTargets(restored, dir, d.recoveryKeyFile, p)
	d.hostESXi = flags.ESXi
	return flags, err
}

func credentialTargets(dir string, flags restoredCredentials) []restoreTarget {
	var targets []restoreTarget
	for _, target := range []struct {
		enabled    bool
		name, path string
	}{
		{flags.GitHub, "github.token", filepath.Join(dir, "github.token")},
		{flags.Key, "recovery.key", filepath.Join(dir, "recovery.key")},
		{flags.ESXi, "esxi.json", "/etc/cliproxy-esxi/esxi.json"},
		{flags.Tunnel, "tunnel-token", "/etc/cliproxy-cloudflared/tunnel-token"},
	} {
		if target.enabled {
			targets = append(targets, restoreTarget{Path: target.path, Archive: "portal/backup/runtime/" + target.name, Kind: "file"})
		}
	}
	return targets
}

func prepareCredentialTargets(restored, dir, keyFile string, p PortalRestorePolicy) (restoredCredentials, error) {
	var flags restoredCredentials
	v, err := readCredentials(restored)
	if err != nil {
		return flags, err
	}
	if !p.defaultHostLayout() {
		return flags, errors.New("自动还原宿主机凭据需要默认部署路径")
	}
	if _, err := readBackupSettings(filepath.Join(restored, filepath.FromSlash(backupSettingsName))); err != nil {
		return flags, errors.New("还原备份凭据需要配套备份设置")
	}
	settings, _ := readBackupSettings(filepath.Join(restored, filepath.FromSlash(backupSettingsName)))
	if settings.Repository != v.Repository {
		return flags, errors.New("备份设置和 GitHub 凭据的仓库不一致")
	}
	flags.GitHub, flags.ESXi, flags.Tunnel = true, v.ESXi != nil, v.CloudflareToken != ""
	key, err := ReadKey(keyFile)
	if err != nil {
		return flags, errors.New("无法保留本次恢复使用的备份密钥")
	}
	flags.Key = true
	data := map[string][]byte{"github.token": []byte(v.GitHubToken + "\n"), "recovery.key": []byte(hex.EncodeToString(key) + "\n")}
	if v.ESXi != nil {
		data["esxi.json"], _ = json.Marshal(v.ESXi)
	}
	if flags.Tunnel {
		data["tunnel-token"] = []byte(v.CloudflareToken + "\n")
	}
	for name, raw := range data {
		if err := atomicWrite(filepath.Join(restored, "portal/backup/runtime", name), raw, 0o600); err != nil {
			return flags, errors.New("恢复凭据无法暂存")
		}
	}
	for _, target := range credentialTargets(dir, flags) {
		if target.Path == filepath.Join(dir, "github.token") || target.Path == filepath.Join(dir, "recovery.key") {
			continue
		}
		if err := (hostInstaller{}).directory(filepath.Dir(target.Path), 0o700); err != nil {
			return flags, err
		}
	}
	return flags, nil
}

func validateAdditionalTargets(targets []restoreTarget) error {
	for i, target := range targets {
		if !filepath.IsAbs(target.Path) || filepath.Clean(target.Path) != target.Path {
			return errors.New("附加恢复目标无效")
		}
		for _, previous := range targets[:i] {
			if previous.Path == target.Path || previous.Kind == "directory" && within(target.Path, previous.Path) {
				return errors.New("附加恢复目标重叠")
			}
		}
	}
	return nil
}
