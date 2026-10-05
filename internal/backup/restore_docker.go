package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type dockerPortal struct {
	policy          PortalRestorePolicy
	rebuild         bool
	images          map[string]string
	rollbackMissing bool
	execute         func(context.Context, ...string) ([]byte, error)
}
type portalInspection struct {
	State struct {
		Running bool
		Health  *struct{ Status string }
	}
	Mounts []struct {
		Name        string
		Type        string
		Source      string
		Destination string
		RW          bool
	}
	Env    []string
	Labels map[string]string
}

func (d *dockerPortal) inspect(ctx context.Context) (portalInspection, error) {
	return d.inspectContainer(ctx, d.policy.Container)
}

func (d *dockerPortal) inspectContainer(ctx context.Context, container string) (portalInspection, error) {
	var info portalInspection
	// Inspect in memory only; never log environment values or raw output.
	data, err := d.output(ctx, "inspect", "--format", `{"State":{{json .State}},"Mounts":{{json .Mounts}},"Env":{{json .Config.Env}},"Labels":{{json .Config.Labels}}}`, container)
	if err != nil || json.Unmarshal(data, &info) != nil {
		return info, errors.New("门户容器状态无法确认")
	}
	return info, nil
}

func (d *dockerPortal) Verify(ctx context.Context, p PortalRestorePolicy) error {
	info, err := d.inspect(ctx)
	if err != nil {
		return err
	}
	data := false
	for _, mount := range info.Mounts {
		if mount.Destination == "/data" && mount.RW && filepath.Clean(mount.Source) == filepath.Dir(p.Database) {
			data = true
		}
	}
	if !data || info.State.Health == nil {
		return errors.New("门户容器挂载或健康检查与恢复策略不一致，拒绝操作")
	}
	database := "/data/portal.db"
	for _, setting := range info.Env {
		if strings.HasPrefix(setting, "PORTAL_DATABASE_PATH=") {
			database = strings.TrimPrefix(setting, "PORTAL_DATABASE_PATH=")
		}
	}
	if database != "/data/"+filepath.Base(p.Database) {
		return errors.New("门户数据库路径与恢复策略不一致，拒绝操作")
	}
	if d.rebuild {
		existing, err := d.containerNames(ctx)
		if err != nil {
			return err
		}
		for _, target := range []struct{ container, service string }{{p.UpstreamContainer, p.UpstreamService}, {p.ManagerContainer, p.ManagerService}} {
			if !existing[target.container] {
				continue
			}
			info, err := d.inspectContainer(ctx, target.container)
			if err != nil || info.Labels["com.docker.compose.project"] != p.ProjectName || info.Labels["com.docker.compose.service"] != target.service {
				return errors.New("上游容器不属于批准的部署项目，拒绝重建")
			}
		}
		if existing[p.ManagerContainer] {
			if err := d.verifyManagerStorage(ctx, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *dockerPortal) Stop(ctx context.Context) error {
	names := []string{d.policy.Container}
	if d.rebuild {
		names = append(names, d.policy.ManagerContainer, d.policy.UpstreamContainer)
	}
	existing, err := d.containerNames(ctx)
	if err != nil {
		return err
	}
	for _, container := range names {
		if !existing[container] {
			continue
		}
		if _, err := d.output(ctx, "stop", "--time", "30", container); err != nil {
			return errors.New("恢复停服失败，未允许替换数据")
		}
		info, err := d.inspectContainer(ctx, container)
		if err != nil || info.State.Running {
			return errors.New("服务尚未停止，不能替换数据")
		}
	}
	return nil
}

func (d *dockerPortal) Start(ctx context.Context) error {
	if d.rebuild && !d.rollbackMissing {
		if err := d.compose(ctx, "up"); err != nil {
			return err
		}
	}
	if d.rollbackMissing {
		existing, err := d.containerNames(ctx)
		if err != nil {
			return err
		}
		for _, name := range []string{d.policy.ManagerContainer, d.policy.UpstreamContainer} {
			if existing[name] {
				if _, err = d.output(ctx, "rm", name); err != nil {
					return errors.New("新建上游服务回退失败")
				}
			}
		}
	}
	if _, err := d.output(ctx, "start", d.policy.Container); err != nil {
		return errors.New("门户启动失败")
	}
	return nil
}

func (d *dockerPortal) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		ready := true
		for _, target := range []struct {
			container, endpoint string
			requireHealth       bool
		}{{d.policy.UpstreamContainer, d.policy.UpstreamHealthURL, false}, {d.policy.ManagerContainer, d.policy.ManagerHealthURL, true}, {d.policy.Container, d.policy.HealthURL, true}} {
			if target.container != d.policy.Container && (!d.rebuild || d.rollbackMissing) {
				continue
			}
			info, err := d.inspectContainer(ctx, target.container)
			if err != nil || !info.State.Running || (target.requireHealth && (info.State.Health == nil || info.State.Health.Status != "healthy")) {
				ready = false
				break
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", target.endpoint, nil)
			response, err := client.Do(req)
			if err != nil {
				ready = false
				break
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				ready = false
				break
			}
		}
		if ready && d.rebuild && !d.rollbackMissing {
			ready = d.managementReady(ctx, client)
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("门户恢复后健康检查未通过")
		case <-time.After(2 * time.Second):
		}
	}
}

// Health endpoints alone do not prove that restored management keys work or
// that CPAMP can reach CPA. Check both authenticated read-only routes as well.
func (d *dockerPortal) managementReady(ctx context.Context, client *http.Client) bool {
	for _, target := range []struct{ health, keyFile, suffix string }{
		{d.policy.UpstreamHealthURL, d.policy.CPAKeyFile, "/"},
		{d.policy.ManagerHealthURL, d.policy.ManagerKeyFile, "/health"},
	} {
		data, err := os.ReadFile(target.keyFile)
		if err != nil || len(data) > 4096 {
			return false
		}
		key := strings.TrimSpace(string(data))
		if key == "" || strings.ContainsAny(key, "\r\n\x00") {
			return false
		}
		endpoint := strings.TrimSuffix(target.health, target.suffix) + "/v0/management/config.yaml"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+key)
		response, err := client.Do(req)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return false
		}
	}
	return true
}

func (d *dockerPortal) containerNames(ctx context.Context) (map[string]bool, error) {
	data, err := d.output(ctx, "ps", "--all", "--format", "{{.Names}}")
	if err != nil {
		return nil, errors.New("Docker 服务不可用")
	}
	names := map[string]bool{}
	for _, name := range strings.Fields(string(data)) {
		names[name] = true
	}
	return names, nil
}
