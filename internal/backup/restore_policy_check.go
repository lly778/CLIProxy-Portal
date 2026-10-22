package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (p PortalRestorePolicy) requireRebuildFields() error {
	// Report field names only, never deployment values or credentials.
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var missing []string
	for _, name := range []string{
		"upstream_config", "auths", "upstream_container", "upstream_health_url",
		"manager_container", "manager_health_url", "manager_database", "manager_data_key",
		"manager_volume", "compose_file", "compose_sha256", "project_name",
		"upstream_service", "manager_service", "upstream_repository", "manager_repository",
		"manager_key_file", "cpa_key_file", "portal_manager_key_file",
	} {
		if strings.TrimSpace(fields[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("后台重建配置缺少字段：%s；请按 ops/backup/restore.example.json 补齐 restore.json 并批准部署模板后重试；普通恢复仅还原门户数据库及备份设置", strings.Join(missing, ", "))
	}
	return nil
}

// Run before decrypting an upload. Rebuild approval comes exclusively from
// trusted on-host policy and template files, never from the uploaded archive.
func loadRestorePolicyForMode(file, mode string) (PortalRestorePolicy, error) {
	p, err := LoadRestorePolicy(file)
	if err != nil {
		return p, err
	}
	if err := p.validate(mode); err != nil {
		return p, err
	}
	if mode == RebuildRestoreMode {
		err = (&dockerPortal{policy: p}).validateProfile()
	}
	return p, err
}

// CheckRestorePolicy checks the installation without pulling images, creating
// volumes, stopping services or reading an uploaded backup. A passing check
// establishes host readiness; archive compatibility is checked during restore.
func CheckRestorePolicy(ctx context.Context, file, mode string) error {
	p, err := loadRestorePolicyForMode(file, mode)
	if err != nil {
		return err
	}
	return (&dockerPortal{policy: p, rebuild: mode == RebuildRestoreMode}).checkRestorePolicy(ctx, mode)
}

func (d *dockerPortal) checkRestorePolicy(ctx context.Context, mode string) error {
	for _, target := range d.policy.targets(mode) {
		parent := filepath.Dir(target.Path)
		real, err := filepath.EvalSymlinks(parent)
		info, statErr := os.Stat(parent)
		if err != nil || statErr != nil || !info.IsDir() || real != parent {
			return fmt.Errorf("恢复目标 %s 的父目录未准备好或经过链接，请先建立实际目标目录", target.Archive)
		}
	}
	if mode == RebuildRestoreMode {
		compiled, err := d.compiledServices(ctx, filepath.Join(filepath.Dir(d.policy.ComposeFile), ".env"))
		if err != nil {
			return err
		}
		if err := d.validateManagerMount(compiled); err != nil {
			return err
		}
		// Verify both services are present or both absent. Do not accept a
		// partial deployment even if the remaining container has valid labels.
		names, err := d.containerNames(ctx)
		if err != nil {
			return err
		}
		if names[d.policy.UpstreamContainer] != names[d.policy.ManagerContainer] {
			return errors.New("上游部署不完整，请管理员先确认残留容器")
		}
	}
	return d.Verify(ctx, d.policy)
}
