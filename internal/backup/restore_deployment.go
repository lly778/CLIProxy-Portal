package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

var imageDigest = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]+@sha256:[a-f0-9]{64}$`)

func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	args = append([]string{"--host", "unix:///var/run/docker.sock"}, args...)
	return exec.CommandContext(ctx, "docker", args...).Output()
}

func (d *dockerPortal) output(ctx context.Context, args ...string) ([]byte, error) {
	if d.execute != nil {
		return d.execute(ctx, args...)
	}
	return dockerOutput(ctx, args...)
}

func pinnedImage(ctx context.Context, container, repository string, output func(context.Context, ...string) ([]byte, error)) (string, error) {
	data, err := output(ctx, "inspect", "--format", "{{.Image}}", container)
	if err != nil {
		return "", errors.New("无法记录已安装的上游镜像版本")
	}
	data, err = output(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", strings.TrimSpace(string(data)))
	if err != nil {
		return "", errors.New("已安装的上游镜像摘要不可用")
	}
	var digests []string
	if json.Unmarshal(data, &digests) != nil {
		return "", errors.New("上游镜像摘要格式无效")
	}
	for _, image := range digests {
		if imageDigest.MatchString(image) && strings.HasPrefix(image, repository+"@sha256:") {
			return image, nil
		}
	}
	return "", errors.New("上游镜像不属于后台批准的仓库")
}

func captureImages(ctx context.Context, p PortalRestorePolicy) (map[string]string, error) {
	return captureImagesWith(ctx, p, dockerOutput)
}

func captureImagesWith(ctx context.Context, p PortalRestorePolicy, output func(context.Context, ...string) ([]byte, error)) (map[string]string, error) {
	cpa, err := pinnedImage(ctx, p.UpstreamContainer, p.UpstreamRepository, output)
	if err != nil {
		return nil, err
	}
	cpamp, err := pinnedImage(ctx, p.ManagerContainer, p.ManagerRepository, output)
	if err != nil {
		return nil, err
	}
	return map[string]string{"cpa": cpa, "cpamp": cpamp}, nil
}

func validateImages(p PortalRestorePolicy, images map[string]string) error {
	if len(images) != 2 {
		return errors.New("备份未记录两个上游的固定镜像版本，不能重建；可使用普通数据库恢复")
	}
	for _, entry := range []struct{ role, repository string }{{"cpa", p.UpstreamRepository}, {"cpamp", p.ManagerRepository}} {
		if entry.repository == "" || !imageDigest.MatchString(images[entry.role]) || !strings.HasPrefix(images[entry.role], entry.repository+"@sha256:") {
			return errors.New("镜像版本不属于批准的固定摘要仓库")
		}
	}
	return nil
}

func (d *dockerPortal) validateProfile() error {
	if err := d.policy.validate(RebuildRestoreMode); err != nil {
		return err
	}
	if err := trustedDeploymentFile(d.policy.ComposeFile); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(d.policy.ComposeFile)
	if err != nil || real != d.policy.ComposeFile {
		return errors.New("后台部署文件不能通过链接重定向")
	}
	data, err := os.ReadFile(d.policy.ComposeFile)
	if err != nil || len(data) > 1<<20 {
		return errors.New("后台部署文件无法读取或过大")
	}
	digest := sha256.Sum256(data)
	if len(d.policy.ComposeSHA256) != 64 || d.policy.ComposeSHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("部署模板与后台批准的模板不一致，请管理员确认")
	}
	return nil
}

// Authenticated backup data is never executed as code. The Compose template
// must exactly match the operator-approved file; restored parameters may not
// alter mounts, commands, ports or container privilege configuration.
func (d *dockerPortal) Prepare(ctx context.Context, restored string) error {
	if err := d.validateProfile(); err != nil {
		return err
	}
	var manifest Manifest
	if err := readJSON(filepath.Join(restored, "manifest.json"), &manifest); err != nil {
		return err
	}
	if err := validateImages(d.policy, manifest.Images); err != nil {
		return err
	}
	approved, err := os.ReadFile(d.policy.ComposeFile)
	if err != nil {
		return err
	}
	archived, err := os.ReadFile(filepath.Join(restored, "upstream/compose.yaml"))
	if err != nil || string(approved) != string(archived) {
		return errors.New("备份部署模板与批准的模板不一致，不能自动执行")
	}
	if err = validateRebuildContent(restored); err != nil {
		return err
	}
	baseline, err := d.compiledServices(ctx, filepath.Join(filepath.Dir(d.policy.ComposeFile), ".env"))
	if err != nil {
		return err
	}
	proposed, err := d.compiledServices(ctx, filepath.Join(restored, "upstream/.env"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(baseline, proposed) {
		return errors.New("备份部署参数改变了批准的端口、挂载或运行权限，请管理员确认")
	}
	if err = d.validateManagerMount(baseline); err != nil {
		return err
	}
	d.images = manifest.Images
	if err = d.compose(ctx, "pull"); err != nil {
		return err
	}
	return d.ensureManagerVolume(ctx)
}

func validateRebuildContent(restored string) error {
	if err := validateManagerBackup(context.Background(), restored, true); err != nil {
		return err
	}
	var keys []string
	for _, name := range []string{"upstream/secrets/cpamp-admin-key", "portal/secrets/cpamp_admin_key", "upstream/secrets/cpa-management-key"} {
		data, err := os.ReadFile(filepath.Join(restored, filepath.FromSlash(name)))
		key := strings.TrimSpace(string(data))
		if err != nil || len(key) < 16 || len(key) > 4096 || strings.ContainsAny(key, "\r\n\x00") {
			return errors.New("备份管理密钥缺失或格式无效")
		}
		keys = append(keys, key)
	}
	if keys[0] != keys[1] {
		return errors.New("门户与 CPAMP 管理密钥不一致")
	}
	data, err := os.ReadFile(filepath.Join(restored, "upstream/cliproxyapi/config.yaml"))
	var config map[string]any
	if err != nil || len(data) > 4<<20 || yaml.Unmarshal(data, &config) != nil || config["port"] != 8317 || config["auth-dir"] != "/root/.cli-proxy-api" {
		return errors.New("CPA 配置与批准的部署端口或 auths 挂载不兼容")
	}
	management, ok := config["remote-management"].(map[string]any)
	secret, valid := management["secret-key"].(string)
	if !ok || !valid || secret == "" || (secret != keys[2] && bcrypt.CompareHashAndPassword([]byte(secret), []byte(keys[2])) != nil) {
		return errors.New("CPA 配置与管理密钥不一致")
	}
	apiKeys, ok := config["api-keys"].([]any)
	if !ok {
		return errors.New("CPA API Key 配置无效")
	}
	for _, key := range apiKeys {
		if value, ok := key.(string); !ok || strings.TrimSpace(value) == "" {
			return errors.New("CPA API Key 配置无效")
		}
	}
	return nil
}

func (d *dockerPortal) compiledServices(ctx context.Context, envFile string) (map[string]any, error) {
	args := d.composeArgs()
	args = append(args, "--env-file", envFile, "config", "--format", "json")
	data, err := d.output(ctx, args...)
	var result struct {
		Services map[string]map[string]any `json:"services"`
		Secrets  map[string]any            `json:"secrets"`
		Networks map[string]any            `json:"networks"`
		Volumes  map[string]any            `json:"volumes"`
	}
	if err != nil || len(data) > 2<<20 || json.Unmarshal(data, &result) != nil {
		return nil, errors.New("备份部署参数无法通过 Compose 校验")
	}
	// Only image versions may differ; secrets are restored through fixed files.
	// Never print the expanded output, which can contain secrets.
	for _, service := range result.Services {
		delete(service, "image")
	}
	return map[string]any{"services": result.Services, "secrets": result.Secrets, "networks": result.Networks, "volumes": result.Volumes}, nil
}

func (d *dockerPortal) OriginalDeployment(ctx context.Context) (deploymentState, error) {
	names, err := d.containerNames(ctx)
	if err != nil {
		return deploymentState{}, err
	}
	if !names[d.policy.UpstreamContainer] && !names[d.policy.ManagerContainer] {
		return deploymentState{Images: d.images, Missing: true}, nil
	}
	if !names[d.policy.UpstreamContainer] || !names[d.policy.ManagerContainer] {
		return deploymentState{}, errors.New("上游部署不完整，请管理员先确认残留容器")
	}
	images, err := captureImagesWith(ctx, d.policy, d.output)
	return deploymentState{Images: images}, err
}

func (d *dockerPortal) DesiredDeployment() deploymentState { return deploymentState{Images: d.images} }

func (d *dockerPortal) RestoreDeployment(state deploymentState) error {
	if err := validateImages(d.policy, state.Images); err != nil {
		return err
	}
	d.images = state.Images
	d.rebuild = true
	d.rollbackMissing = state.Missing
	return nil
}

func (d *dockerPortal) composeArgs() []string {
	return []string{"compose", "--project-name", d.policy.ProjectName, "--project-directory", filepath.Dir(d.policy.ComposeFile), "--file", d.policy.ComposeFile}
}

func (d *dockerPortal) compose(ctx context.Context, action string) error {
	if err := d.validateProfile(); err != nil {
		return err
	}
	if err := validateImages(d.policy, d.images); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "portal-rebuild-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	override := filepath.Join(work, "images.json")
	services := map[string]any{d.policy.UpstreamService: map[string]string{"image": d.images["cpa"]}, d.policy.ManagerService: map[string]string{"image": d.images["cpamp"]}}
	if err = writeJSON(override, map[string]any{"services": services}); err != nil {
		return err
	}
	args := append(d.composeArgs(), "--file", override)
	switch action {
	case "pull":
		args = append(args, "pull", d.policy.UpstreamService, d.policy.ManagerService)
	case "up":
		args = append(args, "up", "--detach", "--no-deps", "--force-recreate", d.policy.UpstreamService, d.policy.ManagerService)
	default:
		return errors.New("不允许的重建操作")
	}
	if _, err = d.output(ctx, args...); err != nil {
		return errors.New("固定版本镜像下载或部署失败，请检查后台任务")
	}
	return nil
}
