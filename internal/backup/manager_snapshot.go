package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const managerPauseName = ".manager-backup-paused"

type managerPause struct {
	Version   int    `json:"version"`
	Container string `json:"container"`
}

func ManagerBackupPending(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, managerPauseName))
	return !os.IsNotExist(err)
}

func openManagerLease(file string) (*os.File, error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = trustedHostFile(file); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (d *dockerPortal) verifyManagerStorage(ctx context.Context, running bool) error {
	if err := d.policy.validate(RebuildRestoreMode); err != nil {
		return err
	}
	info, err := d.inspectContainer(ctx, d.policy.ManagerContainer)
	if err != nil {
		return err
	}
	if (running && !info.State.Running) || info.Labels["com.docker.compose.project"] != d.policy.ProjectName || info.Labels["com.docker.compose.service"] != d.policy.ManagerService {
		return errors.New("CPAMP 容器未运行或不属于批准的部署项目")
	}
	mounted := false
	for _, mount := range info.Mounts {
		if mount.Destination == "/data" && mount.Type == "volume" && mount.Name == d.policy.ManagerVolume && mount.RW && mount.Source == filepath.Dir(d.policy.ManagerDatabase) {
			mounted = true
		}
	}
	if !mounted {
		return errors.New("CPAMP 数据卷与后台备份/恢复策略不一致")
	}
	env := map[string]string{}
	for _, setting := range info.Env {
		key, value, ok := strings.Cut(setting, "=")
		if ok {
			env[key] = value
		}
	}
	if env["USAGE_DB_PATH"] != "/data/usage.sqlite" || env["CPA_MANAGER_DATA_KEY_PATH"] != "/data/data.key" {
		return errors.New("CPAMP 数据库或加密密钥路径与批准的挂载不一致")
	}
	if env["CPA_MANAGER_DATA_KEY"] != "" || env["CPA_MANAGER_DATA_KEY_FILE"] != "" {
		return errors.New("CPAMP 使用外置数据加密密钥，与 data.key 备份策略不一致")
	}
	return d.verifyVolumeUsers(ctx)
}

func (d *dockerPortal) verifyVolumeUsers(ctx context.Context) error {
	data, err := d.output(ctx, "ps", "--all", "--filter", "volume="+d.policy.ManagerVolume, "--format", "{{.Names}}")
	if err != nil {
		return errors.New("无法确认 CPAMP 数据卷的写入者")
	}
	for _, name := range strings.Fields(string(data)) {
		if name != d.policy.ManagerContainer {
			return errors.New("CPAMP 数据卷被其他容器共享，拒绝生成不一致快照或覆盖")
		}
	}
	return nil
}

func (d *dockerPortal) validateManagerMount(compiled map[string]any) error {
	encoded, err := json.Marshal(compiled)
	if err != nil {
		return err
	}
	var cfg struct {
		Services map[string]struct {
			Volumes []struct {
				Type, Source, Target string
				ReadOnly             bool `json:"read_only"`
			}
			Environment map[string]string
		}
		Volumes map[string]struct{ Name string }
	}
	if json.Unmarshal(encoded, &cfg) != nil {
		return errors.New("CPAMP 部署数据卷定义无效")
	}
	manager := cfg.Services[d.policy.ManagerService]
	for _, mount := range manager.Volumes {
		if mount.Target == "/data" && mount.Type == "volume" && !mount.ReadOnly && cfg.Volumes[mount.Source].Name == d.policy.ManagerVolume && manager.Environment["USAGE_DB_PATH"] == "/data/usage.sqlite" && manager.Environment["CPA_MANAGER_DATA_KEY_PATH"] == "/data/data.key" {
			return nil
		}
	}
	return errors.New("批准的 CPAMP 部署模板未挂载指定的数据卷、数据库及 data.key")
}

func (d *dockerPortal) ensureManagerVolume(ctx context.Context) error {
	if err := d.verifyVolumeUsers(ctx); err != nil {
		return err
	}
	names, err := d.output(ctx, "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		return errors.New("无法确认 CPAMP 数据卷")
	}
	exists := false
	for _, name := range strings.Fields(string(names)) {
		if name == d.policy.ManagerVolume {
			exists = true
		}
	}
	if !exists {
		if _, err = d.output(ctx, "volume", "create", d.policy.ManagerVolume); err != nil {
			return errors.New("CPAMP 数据卷创建失败")
		}
	}
	data, err := d.output(ctx, "volume", "inspect", "--format", "{{.Mountpoint}}", d.policy.ManagerVolume)
	if err != nil || strings.TrimSpace(string(data)) != filepath.Dir(d.policy.ManagerDatabase) {
		return errors.New("CPAMP 数据卷实际目录与后台批准的目标不一致")
	}
	return nil
}

func (d *dockerPortal) resumeManager(ctx context.Context) error {
	if err := d.verifyManagerStorage(ctx, false); err != nil {
		return err
	}
	if _, err := d.output(ctx, "start", d.policy.ManagerContainer); err != nil {
		return errors.New("CPAMP 快照后启动失败，请检查宿主机任务")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		info, err := d.inspectContainer(ctx, d.policy.ManagerContainer)
		if err == nil && info.State.Running && info.State.Health != nil && info.State.Health.Status == "healthy" {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("CPAMP 快照后未恢复健康，暂停记录已保留")
		case <-time.After(time.Second):
		}
	}
}

func (d *dockerPortal) withManagerSnapshot(ctx context.Context, state string, collect func() error) (result error) {
	if err := d.verifyManagerStorage(ctx, true); err != nil {
		return err
	}
	marker := filepath.Join(state, managerPauseName)
	if err := os.Mkdir(marker, 0o700); err != nil {
		return errors.New("CPAMP 已有未完成的快照暂停记录")
	}
	lease, err := managerLease(filepath.Join(marker, "lease"))
	if err != nil {
		_ = os.RemoveAll(marker)
		return err
	}
	release := func() {
		if lease != nil {
			lease()
			lease = nil
		}
	}
	defer release()
	if err = writeJSON(filepath.Join(marker, "state.json"), managerPause{Version: 1, Container: d.policy.ManagerContainer}); err != nil {
		release()
		_ = os.RemoveAll(marker)
		return err
	}
	if err = syncDirectory(marker); err != nil {
		release()
		_ = os.RemoveAll(marker)
		return err
	}
	// Persist before stop. Even cancellation or a failed stop must attempt resume.
	defer func() {
		resumeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
		defer cancel()
		if e := d.resumeManager(resumeCtx); e != nil {
			result = errors.Join(result, e)
			return
		}
		release()
		if e := os.RemoveAll(marker); e != nil {
			result = errors.Join(result, e)
		}
	}()
	if _, err = d.output(ctx, "stop", "--time", "30", d.policy.ManagerContainer); err != nil {
		return errors.New("CPAMP 无法暂停，未生成快照")
	}
	info, err := d.inspectContainer(ctx, d.policy.ManagerContainer)
	if err != nil || info.State.Running {
		return errors.New("CPAMP 尚未停止，未生成快照")
	}
	return collect()
}

// The advisory lease survives as a file but the kernel releases it on process
// exit. A later tick can safely resume only the approved manager, even if the
// main job's stale lock still needs operator review. Never resume a live writer
// while an active snapshot still holds the lease.
func recoverManagerSnapshot(ctx context.Context, state, policyFile string) error {
	return recoverManagerSnapshotWith(ctx, state, policyFile, func(p PortalRestorePolicy) *dockerPortal { return &dockerPortal{policy: p} })
}

func recoverManagerSnapshotWith(ctx context.Context, state, policyFile string, controller func(PortalRestorePolicy) *dockerPortal) error {
	marker := filepath.Join(state, managerPauseName)
	if err := trustedHostDirectory(marker); err != nil {
		return err
	}
	lease, err := managerLease(filepath.Join(marker, "lease"))
	if err != nil {
		return err
	}
	release := func() {
		if lease != nil {
			lease()
			lease = nil
		}
	}
	defer release()
	if err = trustedHostFile(filepath.Join(marker, "state.json")); err != nil {
		return err
	}
	var paused managerPause
	if err = readJSON(filepath.Join(marker, "state.json"), &paused); err != nil {
		return err
	}
	p, err := LoadRestorePolicy(policyFile)
	if err != nil {
		return err
	}
	if paused.Version != 1 || paused.Container != p.ManagerContainer {
		return errors.New("CPAMP 暂停记录与后台策略不一致")
	}
	d := controller(p)
	if err = d.resumeManager(ctx); err != nil {
		return err
	}
	release()
	if err = os.RemoveAll(marker); err != nil {
		return err
	}
	s, err := LoadStatus(state)
	if err != nil {
		return err
	}
	s.Running = false
	s.FinishedAt = time.Now().UTC()
	s.Message = "上次备份异常中断，CPAMP 已恢复运行；请检查任务及遗留锁后重试备份"
	if err = writeJSON(filepath.Join(state, "status.json"), s); err != nil {
		return err
	}
	return fmt.Errorf("上次备份中断，CPAMP 已恢复运行；请检查遗留 runner.lock")
}
