package backup

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

// Recreate refreshes single-file secret bind mounts after atomic replacement.
// docker start alone would keep the old inode mounted inside the portal.
func (d *dockerPortal) recreatePortal(ctx context.Context) error {
	h := hostInstaller{}
	if d.installer != nil {
		h = *d.installer
	}
	info, err := d.inspect(ctx)
	if err != nil {
		return err
	}
	labels := info.Labels
	if labels["com.docker.compose.project.working_dir"] != defaultPortalRoot || labels["com.docker.compose.service"] != "cliproxy-portal" || !containerName.MatchString(labels["com.docker.compose.project"]) {
		return errors.New("门户 Compose 安装信息无法确认")
	}
	args := []string{"compose", "--project-name", labels["com.docker.compose.project"], "--project-directory", defaultPortalRoot}
	files := strings.Split(labels["com.docker.compose.project.config_files"], ",")
	if len(files) == 0 || len(files) > 5 {
		return errors.New("门户 Compose 文件列表无效")
	}
	for _, file := range files {
		if (filepath.Clean(filepath.Dir(file)) != filepath.Clean(defaultPortalRoot) && file != defaultPortalRoot+"/ops/compose.restore-monitoring.json") || trustedDeploymentFile(h.path(file)) != nil {
			return errors.New("门户 Compose 文件不属于批准的安装目录")
		}
		if file != defaultPortalRoot+"/ops/compose.restore-monitoring.json" {
			args = append(args, "--file", file)
		}
	}
	file := defaultPortalRoot + "/ops/compose.restore-monitoring.json"
	metrics := d.portalHostMetrics
	if d.hostESXi {
		metrics = "/data/esxi-metrics.json"
	}
	environment := map[string]string{"PORTAL_HOST_METRICS_PATH": metrics, "CPAMP_BASE_URL": "http://host.docker.internal:18317", "CPA_UPSTREAM_URL": strings.Replace(strings.TrimSuffix(d.policy.UpstreamHealthURL, "/"), "127.0.0.1", "host.docker.internal", 1)}
	service := map[string]any{"environment": environment}
	if d.portalAGY || d.hostAGY && !d.agyRollback {
		sessions, memory, err := h.agyContainerLimits()
		if err != nil {
			return err
		}
		environment["PORTAL_AGY_EMBEDDED"] = "true"
		environment["PORTAL_AGY_BASE_URL"] = "http://127.0.0.1:18320"
		environment["PORTAL_AGY_SECRET_FILE"] = "/run/secrets/portal_agy_secret"
		environment["AGY_HOME"] = agyHome
		environment["AGY_COMMAND"] = "/opt/cliproxy-agy/bin/agy"
		environment["AGY_MAX_SESSIONS"] = strconv.Itoa(sessions)
		service["image"] = "cliproxy-portal-runtime:bookworm"
		service["build"] = map[string]any{"context": defaultPortalRoot + "/ops/agy", "dockerfile": "Dockerfile.runtime"}
		service["init"] = true
		service["mem_limit"] = int64(memory) * 1_000_000
		service["volumes"] = []map[string]any{
			{"type": "bind", "source": defaultPortalRoot + "/secrets/portal_agy_secret", "target": "/run/secrets/portal_agy_secret", "read_only": true},
			{"type": "bind", "source": "/opt/cliproxy-agy/bin/agy", "target": "/opt/cliproxy-agy/bin/agy", "read_only": true},
			{"type": "bind", "source": agyHome, "target": agyHome},
		}
	}
	data, err := json.Marshal(map[string]any{"services": map[string]any{"cliproxy-portal": service}})
	if err != nil {
		return err
	}
	if err := h.write(file, data, 0o644, false); err != nil {
		return err
	}
	args = append(args, "--file", file)
	args = append(args, "up", "--detach", "--no-deps", "--force-recreate", "cliproxy-portal")
	if _, err := d.output(ctx, args...); err != nil {
		return errors.New("恢复后重新创建门户容器失败")
	}
	return nil
}
