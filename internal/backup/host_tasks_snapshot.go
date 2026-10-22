package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const hostTasksSnapshotName = "portal/backup/host-tasks.json"

var portalHostUnits = []string{
	"cliproxy-portal-backup.service", "cliproxy-portal-backup.timer", "cliproxy.slice",
	"cliproxy-portal-log-metrics.service", "cliproxy-portal-log-metrics.timer", "cliproxy-esxi-metrics.service",
}

var hostUnitInlineCredential = regexp.MustCompile(`(?im)--(?:token|password|secret|api-key)(?:=|[ \t]+)|(?:^|[ \t"'=])(?:[A-Za-z0-9_]*(?:TOKEN|PASSWORD|SECRET|API_KEY)|GITHUB_PAT)[ \t]*=`)
var hostDropInName = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.conf$`)

type hostTaskFile struct {
	Archive string `json:"archive"`
	Target  string `json:"original_target"`
	Enabled bool   `json:"enabled,omitempty"`
}

type hostTaskInventory struct {
	Version       int            `json:"version"`
	Files         []hostTaskFile `json:"files"`
	Prerequisites []string       `json:"prerequisites"`
}

func allowedHostTaskName(name string) bool {
	if name == hostTasksSnapshotName || name == "portal/host/systemd/cloudflared.service.example" ||
		name == "portal/host/ops/collect-host-log-metrics.py" || name == "portal/host/ops/collect-esxi-metrics.py" {
		return true
	}
	for _, unit := range portalHostUnits {
		base := "portal/host/systemd/" + unit
		if name == base || (strings.HasPrefix(name, base+".d/") && hostDropInName.MatchString(strings.TrimPrefix(name, base+".d/"))) {
			return true
		}
	}
	return false
}

func snapshotHostTasks(p PortalRestorePolicy, sources []Source, work string) ([]Source, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	portalRoot := filepath.Dir(filepath.Dir(p.Database))
	for _, source := range sources {
		if source.Name == "portal/compose.prebuilt.yaml" {
			portalRoot = filepath.Dir(source.Path)
			break
		}
	}
	return collectHostTaskSources(portalRoot, "/etc/systemd/system", work)
}

// Copy only this application's known tasks. Never archive arbitrary systemd
// units or host credentials, and never execute archived task definitions.
func collectHostTaskSources(portalRoot, unitDir, work string) ([]Source, error) {
	inventory := hostTaskInventory{Version: 1, Prerequisites: []string{
		"先安装 Docker/Compose、Python 3、门户程序；使用隧道时另行安装 cloudflared。",
		"核对原路径和任务内容后安装 systemd 文件，daemon-reload 后按原启用状态启动。",
		"GitHub 令牌、备份恢复密钥、ESXi 凭据及 Cloudflare Tunnel 凭据不在此任务快照内，需单独配置。",
	}}
	var sources []Source
	add := func(name, target string, data []byte, enabled bool) error {
		file := filepath.Join(work, fmt.Sprintf("host-task-%02d.snapshot", len(sources)))
		if err := atomicWrite(file, data, 0o600); err != nil {
			return err
		}
		sources = append(sources, Source{Path: file, Name: name, Required: true})
		inventory.Files = append(inventory.Files, hostTaskFile{Archive: name, Target: target, Enabled: enabled})
		return nil
	}
	copyFile := func(file, name string, unit, enabled bool) error {
		if err := trustedDeploymentFile(file); err != nil {
			return fmt.Errorf("宿主机任务文件 %s 不存在或权限不安全", name)
		}
		info, err := os.Stat(file)
		if err != nil || info.Size() > 1<<20 {
			return fmt.Errorf("宿主机任务文件 %s 无法读取或过大", name)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("宿主机任务文件 %s 无法读取", name)
		}
		if unit && hostUnitInlineCredential.Match(data) {
			return errors.New("门户宿主机任务含内联凭据，请改用受保护的外部凭据文件后再备份")
		}
		return add(name, file, data, enabled)
	}
	for _, unit := range portalHostUnits {
		file := filepath.Join(unitDir, unit)
		if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, errors.New("宿主机任务无法检查")
		}
		enabled := false
		for _, target := range []string{"timers.target", "multi-user.target", "slices.target"} {
			if _, err := os.Lstat(filepath.Join(unitDir, target+".wants", unit)); err == nil {
				enabled = true
			}
		}
		if err := copyFile(file, "portal/host/systemd/"+unit, true, enabled); err != nil {
			return nil, err
		}
		dropIns, err := os.ReadDir(file + ".d")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("宿主机任务覆盖配置无法读取")
		}
		for _, entry := range dropIns {
			if hostDropInName.MatchString(entry.Name()) {
				if len(sources) >= 64 {
					return nil, errors.New("宿主机任务文件过多")
				}
				if err := copyFile(filepath.Join(file+".d", entry.Name()), "portal/host/systemd/"+unit+".d/"+entry.Name(), true, false); err != nil {
					return nil, err
				}
			}
		}
		var script string
		switch unit {
		case "cliproxy-portal-log-metrics.service":
			script = "collect-host-log-metrics.py"
		case "cliproxy-esxi-metrics.service":
			script = "collect-esxi-metrics.py"
		}
		if script != "" {
			if err := copyFile(filepath.Join(portalRoot, "ops", script), "portal/host/ops/"+script, false, false); err != nil {
				return nil, err
			}
		}
	}
	// Tunnel units often contain a live token in ExecStart. Export a clean
	// token-file template instead of reading or copying that command line.
	for _, unit := range []string{"cloudflared.service", "cloudflared-cliproxy.service"} {
		if _, err := os.Lstat(filepath.Join(unitDir, unit)); err == nil {
			if err := add("portal/host/systemd/cloudflared.service.example", filepath.Join(unitDir, "cloudflared.service"), []byte(cloudflaredMigrationUnit), false); err != nil {
				return nil, err
			}
			break
		}
	}
	file := filepath.Join(work, "host-tasks.json")
	if err := writeJSON(file, inventory); err != nil {
		return nil, err
	}
	return append(sources, Source{Path: file, Name: hostTasksSnapshotName, Required: true}), nil
}

const cloudflaredMigrationUnit = `# Migration template; supply a separately protected tunnel token file.
[Unit]
Description=Cloudflare Tunnel for CLIProxy Portal
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/cloudflared --no-autoupdate tunnel --edge-ip-version 4 --protocol http2 --metrics 127.0.0.1:20241 run --token-file /etc/cliproxy-cloudflared/tunnel-token
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`
