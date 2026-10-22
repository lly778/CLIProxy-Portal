package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const hostRestoreName = ".host-restore-pending"

type hostRestorePlan struct {
	Version int                 `json:"version"`
	Policy  PortalRestorePolicy `json:"policy"`
	Ready   bool                `json:"ready"`
	ESXi    bool                `json:"esxi"`
	Tunnel  bool                `json:"tunnel"`
}

func HostRecoveryPending(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, hostRestoreName))
	return !errors.Is(err, os.ErrNotExist)
}

func prepareHostRestore(dir string, p PortalRestorePolicy, restored string) error {
	if !p.defaultHostLayout() {
		return errors.New("自动安装宿主机任务需要默认部署路径")
	}
	v, err := readCredentials(restored)
	if err != nil {
		return err
	}
	plan := hostRestorePlan{Version: 1, Policy: p, ESXi: v.ESXi != nil, Tunnel: v.CloudflareToken != ""}
	// Archived units and scripts are references only. The pending record holds
	// no credentials and installation always uses compiled-in program assets.
	path := filepath.Join(dir, hostRestoreName)
	if err := os.Mkdir(path, 0o700); err != nil {
		return errors.New("存在尚未完成的宿主机恢复任务")
	}
	if err := writeJSON(filepath.Join(path, "plan.json"), plan); err != nil {
		_ = os.RemoveAll(path)
		return err
	}
	return nil
}

func markHostRestoreReady(dir string) error {
	if !HostRecoveryPending(dir) {
		return nil
	}
	file := filepath.Join(dir, hostRestoreName, "plan.json")
	var plan hostRestorePlan
	if err := readJSON(file, &plan); err != nil {
		return err
	}
	plan.Ready = true
	return writeJSON(file, plan)
}

func resumeHostRestore(ctx context.Context, dir string) error {
	return resumeHostRestoreWith(ctx, dir, hostInstaller{})
}

func resumeHostTick(ctx context.Context, dir string) error {
	file := filepath.Join(dir, "runner.lock")
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("备份或恢复任务正在执行")
	}
	_ = f.Close()
	defer os.Remove(file)
	return resumeHostRestore(ctx, dir)
}

func resumeHostRestoreWith(ctx context.Context, dir string, installer hostInstaller) error {
	path := filepath.Join(dir, hostRestoreName)
	if trustedHostDirectory(path) != nil {
		return errors.New("宿主机恢复记录权限无效")
	}
	var plan hostRestorePlan
	if readJSON(filepath.Join(path, "plan.json"), &plan) != nil || plan.Version != 1 || !plan.Policy.defaultHostLayout() {
		return errors.New("宿主机恢复记录无效")
	}
	status, err := LoadRestoreStatus(dir)
	if err != nil {
		return err
	}
	if !plan.Ready {
		// No committed data transaction authorized installing services. A crash
		// before commit must never install the pending plan on its own.
		status.Running = false
		status.Phase = "failed"
		status.FailedPhase = "preflight"
		status.FinishedAt = time.Now().UTC()
		status.Message = "恢复在数据提交前中断，未安装宿主机任务；请重新提交恢复"
		if err := writeJSON(filepath.Join(dir, "restore-status.json"), status); err != nil {
			return err
		}
		return os.RemoveAll(path)
	}
	status.Running = true
	status.Phase = "install_host"
	status.Message = "正在安装并启用宿主机配套任务"
	status.PhaseStartedAt = time.Now().UTC()
	if err := writeJSON(filepath.Join(dir, "restore-status.json"), status); err != nil {
		return err
	}
	if err := installer.installTasks(ctx, plan.ESXi, plan.Tunnel); err != nil {
		status.Running = false
		status.Phase = "host_pending"
		status.FailedPhase = "install_host"
		status.FinishedAt = time.Now().UTC()
		status.Message = "数据已恢复；宿主机任务安装尚未完成，后台将重试：" + err.Error()
		_ = writeJSON(filepath.Join(dir, "restore-status.json"), status)
		return err
	}
	// The current runner may be executing under this very timer's service.
	// Enable the timer without restarting the running backup oneshot.
	if err := installer.command(ctx, "systemctl", "enable", "--now", "cliproxy-portal-backup.timer"); err != nil {
		status.Running = false
		status.Phase = "host_pending"
		status.FailedPhase = "install_host"
		status.Message = "数据已恢复，备份定时任务启用失败，后台将重试"
		_ = writeJSON(filepath.Join(dir, "restore-status.json"), status)
		return err
	}
	status.Running = false
	status.Phase = "complete"
	status.FailedPhase = ""
	status.FinishedAt = time.Now().UTC()
	status.PhaseStartedAt = status.FinishedAt
	status.Message = "CPA、CPAMP 及数据已恢复，宿主机任务和配套凭据已还原；请重新登录"
	var missing []string
	if !plan.ESXi {
		missing = append(missing, "备份未含宿主机监测凭据，保留当前监测配置")
	}
	if !plan.Tunnel {
		missing = append(missing, "备份未含隧道凭据，保留当前入口配置")
	}
	if len(missing) > 0 {
		status.Warning = strings.Join(missing, "；")
	}
	if err := writeJSON(filepath.Join(dir, "restore-status.json"), status); err != nil {
		return err
	}
	return os.RemoveAll(path)
}
