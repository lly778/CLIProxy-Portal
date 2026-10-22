package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const restorePolicySnapshotName = "portal/backup/restore.json"
const sourcesSnapshotName = "portal/backup/sources.json"

// Save typed host configuration, not arbitrary raw JSON that might contain
// unrelated credentials. The snapshots are migration references, never an
// instruction to overwrite the target host's approved execution policy.
func snapshotDeploymentConfig(sources []Source, policy *PortalRestorePolicy, work string) ([]Source, error) {
	for _, source := range sources {
		if source.Name == restorePolicySnapshotName || source.Name == sourcesSnapshotName || source.Name == backupSettingsName || allowedHostTaskName(source.Name) {
			return nil, errors.New("后台配置快照由备份任务自动生成，请勿写入 sources.json")
		}
	}
	if policy != nil && (policy.UpstreamContainer != "" || policy.ManagerContainer != "") {
		if err := (&dockerPortal{policy: *policy}).validateProfile(); err != nil {
			return nil, err
		}
		// A complete policy is not enough: every rebuild target must actually
		// be included from the approved location as a required source.
		for _, target := range policy.targets(RebuildRestoreMode) {
			matched := false
			for _, source := range sources {
				if source.Name == target.Archive {
					matched = source.Path == target.Path && source.Required &&
						source.Directory == (target.Kind == "directory") &&
						source.SQLite == (target.Kind == "database" || target.Kind == "manager-database")
				}
			}
			if !matched {
				return nil, fmt.Errorf("完整重建备份缺少必需来源或与恢复策略不一致：%s；请检查 sources.json", target.Archive)
			}
		}
	}
	var result []Source
	for _, snapshot := range []struct {
		name, file string
		value      any
	}{
		{sourcesSnapshotName, "backup-sources.json", sources},
		{restorePolicySnapshotName, "backup-restore-policy.json", policy},
	} {
		if snapshot.name == restorePolicySnapshotName && policy == nil {
			continue
		}
		file := filepath.Join(work, snapshot.file)
		if err := writeJSON(file, snapshot.value); err != nil {
			return nil, err
		}
		result = append(result, Source{Path: file, Name: snapshot.name, Required: true})
	}
	return result, nil
}

// Validate immutable staged bytes as well as extracted archives. Metadata may
// describe a different operating system, so never interpret its paths as local
// destinations or replace the target's policy while reading a backup.
func validateDeploymentSnapshot(dir string) error {
	var sources []Source
	sourceFile := filepath.Join(dir, filepath.FromSlash(sourcesSnapshotName))
	if err := readDeploymentSnapshot(sourceFile, &sources); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := validateSources(sources); err != nil {
		return errors.New("备份来源清单无效")
	}
	var p PortalRestorePolicy
	if err := readDeploymentSnapshot(filepath.Join(dir, filepath.FromSlash(restorePolicySnapshotName)), &p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // Backups made before configuration snapshots remain valid.
		}
		return err
	}
	if sources == nil {
		return errors.New("备份包含恢复策略但缺少配套来源清单")
	}
	if p.UpstreamContainer == "" && p.ManagerContainer == "" {
		return nil // An explicitly database-only installation has no rebuild profile.
	}
	if err := p.requireRebuildFields(); err != nil {
		return errors.New("备份中的重建配置清单不完整")
	}
	for _, target := range p.targets(RebuildRestoreMode) {
		matched := false
		for _, source := range sources {
			if source.Name == target.Archive {
				matched = source.Required && source.Directory == (target.Kind == "directory") &&
					source.SQLite == (target.Kind == "database" || target.Kind == "manager-database")
			}
		}
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(target.Archive)))
		if !matched || err != nil || (target.Kind == "directory" && !info.IsDir()) || (target.Kind != "directory" && !info.Mode().IsRegular()) {
			return fmt.Errorf("备份重建资料不完整：%s", target.Archive)
		}
	}
	compose, err := os.ReadFile(filepath.Join(dir, "upstream/compose.yaml"))
	if err != nil || len(compose) > 1<<20 {
		return errors.New("备份中的重建配置缺少部署模板")
	}
	digest := sha256.Sum256(compose)
	if p.ComposeSHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("备份的部署模板与恢复策略 SHA256 不一致，请检查模板变更后重新备份")
	}
	return nil
}

func readDeploymentSnapshot(file string, value any) error {
	info, err := os.Lstat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("备份部署配置清单必须是受限大小的普通文件")
	}
	if err := readJSON(file, value); err != nil {
		return errors.New("备份部署配置清单格式无效")
	}
	return nil
}
