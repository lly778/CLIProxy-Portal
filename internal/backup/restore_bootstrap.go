package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	portalops "cliproxy-portal/ops"
)

func (d *dockerPortal) bootstrapDeployment(ctx context.Context, restored string) error {
	p := d.policy
	if p.Bootstrap != autoBootstrap || !p.defaultHostLayout() {
		return errors.New("新机自动部署策略无效，请先运行 backup install-host")
	}
	compiled, err := defaultRestorePolicy()
	if err != nil || compiled.ComposeSHA256 != p.ComposeSHA256 {
		return errors.New("自动部署策略与程序内置模板不一致")
	}
	approved, _ := portalops.HostAssets.ReadFile("backup/upstream.approved.yaml")
	archived, err := os.ReadFile(filepath.Join(restored, "upstream/compose.yaml"))
	if err != nil || string(archived) != string(approved) {
		return errors.New("备份模板不受新机自动部署支持，请管理员批准该部署配置")
	}
	if err := validateRebuildContent(restored); err != nil {
		return err
	}
	var manifest Manifest
	if readJSON(filepath.Join(restored, "manifest.json"), &manifest) != nil || validateImages(p, manifest.Images) != nil {
		return errors.New("备份缺少批准的固定镜像版本")
	}
	data, err := os.ReadFile(filepath.Join(restored, "upstream/.env"))
	if err != nil || validateBootstrapEnv(string(data), p) != nil {
		return errors.New("备份部署参数不符合内置部署范围")
	}
	names, err := d.containerNames(ctx)
	if err != nil {
		return err
	}
	if names[p.UpstreamContainer] || names[p.ManagerContainer] {
		return errors.New("已存在上游容器但缺少部署模板，请管理员先核对现有部署")
	}
	h := hostInstaller{}
	if d.installer != nil {
		h = *d.installer
	}
	for _, dir := range []string{filepath.Dir(p.ComposeFile), filepath.Dir(p.UpstreamConfig), p.Auths, filepath.Join(filepath.Dir(p.UpstreamConfig), "logs"), filepath.Dir(p.ManagerKeyFile)} {
		if err := h.directory(dir, 0o700); err != nil {
			return err
		}
	}
	if err := h.write(filepath.Join(filepath.Dir(p.ComposeFile), ".env"), data, 0o600, true); err != nil {
		return err
	}
	if err := h.write(p.ComposeFile, approved, 0o600, true); err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "CPA_PORT="); ok {
			parts := strings.Split(value, ":")
			port := parts[len(parts)-1]
			p.UpstreamHealthURL = "http://127.0.0.1:" + port + "/"
		}
	}
	raw, _ := json.MarshalIndent(p, "", "  ")
	if err := h.write(defaultPolicyFile, raw, 0o600, false); err != nil {
		return err
	}
	d.policy = p
	return nil
}

func validateBootstrapEnv(data string, p PortalRestorePolicy) error {
	values := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || values[name] != "" || strings.ContainsAny(value, "$`\r\x00\"'\\") {
			return errors.New("部署环境参数无效")
		}
		values[name] = value
	}
	if len(values) != 5 || values["COMPOSE_PROJECT_NAME"] != p.ProjectName {
		return errors.New("部署项目参数无效")
	}
	for key, port := range map[string]string{"CPA_PORT": "8317", "CPAMP_PORT": "18317"} {
		v := values[key]
		if key == "CPA_PORT" && v == "127.0.0.1:18319" {
			continue
		}
		if v != port && v != "127.0.0.1:"+port && v != "0.0.0.0:"+port {
			return errors.New("部署端口不受支持")
		}
	}
	for name, repository := range map[string]string{"CPA_IMAGE": p.UpstreamRepository, "CPAMP_IMAGE": p.ManagerRepository} {
		v := values[name]
		if !imageDigest.MatchString(v) || !strings.HasPrefix(v, repository+"@sha256:") {
			return errors.New("部署镜像未固定摘要")
		}
	}
	return nil
}
