package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	portalops "cliproxy-portal/ops"
)

const defaultPortalRoot = "/root/cliproxy-portal"
const defaultPolicyFile = "/etc/cliproxy-portal-backup/restore.json"
const autoBootstrap = "builtin-cpamp-v1"

type hostInstaller struct {
	root string // Test-only filesystem root; production uses absolute host paths.
	run  func(context.Context, ...string) error
}

func (h hostInstaller) path(name string) string {
	if h.root != "" {
		return filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(name, "/")))
	}
	return name
}

func (h hostInstaller) command(ctx context.Context, args ...string) error {
	if h.run != nil {
		return h.run(ctx, args...)
	}
	if err := exec.CommandContext(ctx, args[0], args[1:]...).Run(); err != nil {
		// Command output may contain host configuration or network credentials.
		return fmt.Errorf("安装步骤 %s 执行失败，请检查宿主机任务", filepath.Base(args[0]))
	}
	return nil
}

func (h hostInstaller) directory(name string, mode os.FileMode) error {
	name = h.path(name)
	// Check every existing component before MkdirAll. Never follow a portal-
	// writable symlink into another directory during privileged installation.
	for p := name; p != filepath.Dir(p); p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS == "linux" && info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return errors.New("宿主机安装目录含链接或可被其他用户修改")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("宿主机安装目录无法检查")
		}
	}
	if err := os.MkdirAll(name, mode); err != nil {
		return errors.New("宿主机安装目录无法建立")
	}
	return nil
}

func (h hostInstaller) write(name string, data []byte, mode os.FileMode, onlyMissing bool) error {
	if err := h.directory(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	file := h.path(name)
	if info, err := os.Lstat(file); err == nil {
		if !info.Mode().IsRegular() || runtime.GOOS == "linux" && info.Mode().Perm()&0o022 != 0 {
			return errors.New("宿主机安装目标不是安全的普通文件")
		}
		if onlyMissing {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("宿主机安装文件无法检查")
	}
	return atomicWrite(file, data, mode)
}

func (h hostInstaller) asset(asset, target string, onlyMissing bool) error {
	data, err := portalops.HostAssets.ReadFile(asset)
	if err != nil {
		return errors.New("内置宿主机安装资源缺失")
	}
	return h.write(target, data, 0o644, onlyMissing)
}

func defaultRestorePolicy() (PortalRestorePolicy, error) {
	var p PortalRestorePolicy
	data, err := portalops.HostAssets.ReadFile("backup/restore.example.json")
	if err != nil || json.Unmarshal(data, &p) != nil {
		return p, errors.New("内置恢复策略无法读取")
	}
	compose, err := portalops.HostAssets.ReadFile("backup/upstream.approved.yaml")
	if err != nil {
		return p, err
	}
	digest := sha256.Sum256(compose)
	p.ComposeSHA256 = hex.EncodeToString(digest[:])
	p.Bootstrap = autoBootstrap
	return p, nil
}

func (p PortalRestorePolicy) defaultHostLayout() bool {
	return p.Database == defaultPortalRoot+"/data/portal.db" && p.Container == "cliproxy-portal" &&
		p.ComposeFile == "/root/cpa-manager-plus/compose.yaml" && p.ManagerVolume == "cpamp_cpa-manager-plus-data" &&
		p.ProjectName == "cpamp" && p.resourceCgroupParent() == "cliproxy.slice"
}

// InstallHost is part of portal installation, before any restore is uploaded.
// It installs the privileged queue consumer without giving Docker to the web
// container. Existing policy, credentials, schedule and data remain untouched.
func InstallHost(ctx context.Context) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("宿主机初始化需要在 Linux 上以 root 运行")
	}
	return (hostInstaller{}).install(ctx)
}

func (h hostInstaller) install(ctx context.Context) error {
	for _, dir := range []string{defaultPortalRoot + "/ops", "/root/cpa-manager-plus/secrets", "/root/cpa-manager-plus/cliproxyapi/auths", "/root/cpa-manager-plus/cliproxyapi/logs", "/etc/cliproxy-portal-backup", "/etc/cliproxy-esxi", "/etc/cliproxy-cloudflared"} {
		if err := h.directory(dir, 0o700); err != nil {
			return err
		}
	}
	state := h.path(defaultPortalRoot + "/data/backup")
	dataDir := h.path(defaultPortalRoot + "/data")
	if _, err := os.Lstat(dataDir); errors.Is(err, os.ErrNotExist) {
		if err := h.directory(defaultPortalRoot+"/data", 0o700); err != nil {
			return err
		}
		if h.root == "" {
			if err := os.Chown(dataDir, 10001, 10001); err != nil {
				return errors.New("无法设置门户数据目录权限")
			}
		}
	}
	if _, err := os.Lstat(state); errors.Is(err, os.ErrNotExist) {
		if err := h.directory(defaultPortalRoot+"/data/backup", 0o700); err != nil {
			return err
		}
		if h.root == "" {
			if err := os.Chown(state, 10001, 10001); err != nil {
				return errors.New("无法设置门户备份队列权限")
			}
		}
	}
	if err := h.command(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return err
	}
	if err := h.command(ctx, "docker", "compose", "version"); err != nil {
		return err
	}
	if err := h.command(ctx, "python3", "--version"); err != nil {
		// Python is used only by the two in-tree read-only collectors. No
		// downloaded shell scripts or third-party pip dependencies are needed.
		if err := h.command(ctx, "apt-get", "update"); err != nil {
			return err
		}
		if err := h.command(ctx, "apt-get", "install", "-y", "--no-install-recommends", "python3", "ca-certificates"); err != nil {
			return err
		}
	}
	p, err := defaultRestorePolicy()
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	if err := h.write(defaultPolicyFile, data, 0o600, true); err != nil {
		return err
	}
	if err := h.asset("backup/sources.example.json", "/etc/cliproxy-portal-backup/sources.json", true); err != nil {
		return err
	}
	if err := h.installTasks(ctx, false, false); err != nil {
		return err
	}
	return h.command(ctx, "systemctl", "enable", "--now", "cliproxy-portal-backup.timer")
}

func (h hostInstaller) installTasks(ctx context.Context, esxi, tunnel bool) error {
	assets := map[string]string{
		"cliproxy.slice":                          "/etc/systemd/system/cliproxy.slice",
		"backup/cliproxy-portal-backup.service":   "/etc/systemd/system/cliproxy-portal-backup.service",
		"backup/cliproxy-portal-backup.timer":     "/etc/systemd/system/cliproxy-portal-backup.timer",
		"cliproxy-portal-log-metrics.service":     "/etc/systemd/system/cliproxy-portal-log-metrics.service",
		"cliproxy-portal-log-metrics.timer":       "/etc/systemd/system/cliproxy-portal-log-metrics.timer",
		"collect-host-log-metrics.py":             defaultPortalRoot + "/ops/collect-host-log-metrics.py",
		"collect-esxi-metrics.py":                 defaultPortalRoot + "/ops/collect-esxi-metrics.py",
		"resources/cliproxy-esxi-metrics.service": "/etc/systemd/system/cliproxy-esxi-metrics.service",
	}
	for asset, target := range assets {
		if err := h.asset(asset, target, false); err != nil {
			return err
		}
	}
	if tunnel {
		if err := h.installCloudflared(ctx); err != nil {
			return err
		}
		unit := strings.ReplaceAll(cloudflaredMigrationUnit, "/usr/bin/cloudflared", "/usr/local/bin/cloudflared")
		if err := h.write("/etc/systemd/system/cliproxy-portal-tunnel.service", []byte(unit), 0o644, false); err != nil {
			return err
		}
	}
	if err := h.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	units := []string{"cliproxy.slice", "cliproxy-portal-log-metrics.timer"}
	if esxi {
		units = append(units, "cliproxy-esxi-metrics.service")
	}
	if tunnel {
		units = append(units, "cliproxy-portal-tunnel.service")
	}
	for _, unit := range units {
		if unit == "cliproxy-portal-tunnel.service" {
			for _, old := range []string{"cloudflared-cliproxy.service", "cloudflared.service"} {
				if h.command(ctx, "systemctl", "is-active", "--quiet", old) == nil {
					if err := h.command(ctx, "systemctl", "stop", old); err != nil {
						return err
					}
				}
			}
		}
		if err := h.command(ctx, "systemctl", "enable", "--now", unit); err != nil {
			return err
		}
		if unit == "cliproxy-esxi-metrics.service" || unit == "cliproxy-portal-tunnel.service" {
			if err := h.command(ctx, "systemctl", "restart", unit); err != nil {
				return err
			}
		}
	}
	return nil
}
