package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	portalops "cliproxy-portal/ops"
)

// Fixed official release, checksums from Google's platform updater manifests.
// No download address or executable is ever taken from an uploaded archive.
var agyReleases = map[string]struct{ URL, SHA512 string }{
	"amd64": {"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.4-5037094733742080/linux-x64/cli_linux_x64.tar.gz", "1431a409ef7c7bce23f4abf03f988afab728ba87d91d24ed7db0d5eea3ab71a4b590bf0bf2c79cec84f79b32b674ea38df0417356ce8d5d4f698cd62ea9ee6be"},
	"arm64": {"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.4-5037094733742080/linux-arm/cli_linux_arm64.tar.gz", "564b9e6841ab4631e370d674c52f6eac75fa066fd6f61b1af39cb186dc57d835921d68e598293c7e5237d5605b90c0d3977805c7890f99dd8d9cc2e7471d8ff8"},
}

func (h hostInstaller) agyGateway(ctx context.Context) (string, error) {
	var b []byte
	var err error
	args := []string{"network", "inspect", "cpamp_default", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}"}
	if h.output != nil {
		b, err = h.output(ctx, args...)
	} else {
		b, err = dockerOutput(ctx, args...)
	}
	if err != nil {
		return "", errors.New("AGY 恢复无法读取 Docker 私网网关")
	}
	for _, field := range strings.Fields(string(b)) {
		ip := net.ParseIP(field)
		if ip != nil && ip.To4() != nil && ip.IsPrivate() {
			return ip.String(), nil
		}
	}
	return "", errors.New("AGY 恢复需要 Docker IPv4 私网网关")
}

func (h hostInstaller) installAGY(ctx context.Context) error {
	var config struct {
		MaxSessions int  `json:"max_sessions"`
		MemoryMiB   int  `json:"memory_mib"`
		LoggedIn    bool `json:"logged_in"`
	}
	if readDeploymentSnapshot(h.path(agyConfigPath), &config) != nil || config.MaxSessions < 1 || config.MaxSessions > 16 || config.MemoryMiB < 128 || config.MemoryMiB > 4096 {
		return errors.New("AGY 恢复配置缺失或无效")
	}
	secret, err := privateAGYFile(h.path(agySecretPath), true)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(secret))) < 32 {
		return errors.New("AGY 恢复密钥无效")
	}
	gateway, err := h.agyGateway(ctx)
	if err != nil {
		return err
	}
	if err = h.command(ctx, "id", "-u", "portal-agy"); err != nil {
		return errors.New("AGY 专用用户缺失，请先运行 backup install-host 更新恢复后台")
	}
	for _, dir := range []string{agyHome, agyHome + "/agy", agyHome + "/workspace", "/opt/cliproxy-agy/bin", "/etc/cliproxy-agy"} {
		if err = h.directory(dir, 0700); err != nil {
			return err
		}
	}
	// The bridge comes from the trusted running portal build, never the archive.
	if h.root == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(exe)
		if err != nil {
			return errors.New("无法安装 AGY 桥接程序")
		}
		if err = h.write("/opt/cliproxy-agy/bin/portal-agy", data, 0755, false); err != nil {
			return err
		}
	} else if err = h.write("/opt/cliproxy-agy/bin/portal-agy", []byte("test-built-portal"), 0755, false); err != nil {
		return err
	}
	if h.agyDownload != nil {
		err = h.agyDownload(ctx)
	} else {
		err = h.installOfficialAGY(ctx)
	}
	if err != nil {
		return err
	}
	if !config.LoggedIn {
		if err = os.Remove(h.path(agyTokenPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("无法清理旧 AGY 登录态")
		}
	} else if _, err = privateAGYFile(h.path(agyTokenPath), true); err != nil {
		return err
	}
	// No session restore: make previous response chains unusable on the target.
	cache := h.path(agyHome + "/portal-sessions.db")
	if info, e := os.Lstat(cache); e == nil {
		if !info.Mode().IsRegular() {
			return errors.New("AGY 会话索引不是普通文件")
		}
		db, e := sql.Open("sqlite", cache)
		if e != nil {
			return errors.New("AGY 旧会话索引无法打开")
		}
		_, e = db.ExecContext(ctx, `BEGIN IMMEDIATE; UPDATE sessions SET ready=0; DELETE FROM responses; COMMIT;`)
		_ = db.Close()
		if e != nil {
			return errors.New("AGY 旧会话索引无法失效")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return errors.New("AGY 会话索引无法检查")
	}
	for _, file := range []string{agyHome, agyHome + "/agy", agyHome + "/workspace", agyTokenPath, agyHome + "/agy/installation_id"} {
		if _, e := os.Lstat(h.path(file)); errors.Is(e, os.ErrNotExist) {
			continue
		}
		if err = h.command(ctx, "chown", "portal-agy:portal-agy", file); err != nil {
			return err
		}
	}
	if err = h.command(ctx, "chown", "10001:10001", defaultPortalRoot+"/secrets/portal_agy_secret"); err != nil {
		return err
	}
	for _, dir := range []string{"/opt/cliproxy-agy", "/opt/cliproxy-agy/bin"} {
		if err = os.Chmod(h.path(dir), 0755); err != nil {
			return err
		}
	}
	runtimeEnv := fmt.Sprintf("PORTAL_AGY_LISTEN_ADDR=%s:18320\nAGY_COMMAND=/opt/cliproxy-agy/bin/agy\nAGY_HOME=%s\nAGY_MAX_SESSIONS=%d\n", gateway, agyHome, config.MaxSessions)
	if err = h.write("/etc/cliproxy-agy/runtime.env", []byte(runtimeEnv), 0600, false); err != nil {
		return err
	}
	unit, err := portalops.AGYAssets.ReadFile("agy/cliproxy-agy.service")
	if err != nil {
		return err
	}
	unitText := strings.Replace(string(unit), "ExecStart=/opt/cliproxy-agy/bin/portal-agy", "ExecStart=/opt/cliproxy-agy/bin/portal-agy agy-service", 1)
	unitText = strings.Replace(unitText, "MemoryMax=768M", fmt.Sprintf("MemoryMax=%dM", config.MemoryMiB), 1)
	if err = h.write("/etc/systemd/system/cliproxy-agy.service", []byte(unitText), 0644, false); err != nil {
		return err
	}
	if err = h.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err = h.command(ctx, "systemctl", "enable", "cliproxy-agy.service"); err != nil {
		return err
	}
	if err = h.command(ctx, "systemctl", "restart", "cliproxy-agy.service"); err != nil {
		return err
	}
	return h.command(ctx, "systemctl", "is-active", "--quiet", "cliproxy-agy.service")
}

func (h hostInstaller) prepareAGYHost(ctx context.Context) error {
	for _, dir := range []string{"/etc/cliproxy-agy", "/opt/cliproxy-agy/bin", agyHome, agyHome + "/agy", agyHome + "/workspace"} {
		if err := h.directory(dir, 0700); err != nil {
			return err
		}
	}
	if err := h.command(ctx, "id", "-u", "portal-agy"); err != nil {
		if err = h.command(ctx, "useradd", "--system", "--user-group", "--home-dir", agyHome, "--shell", "/usr/sbin/nologin", "portal-agy"); err != nil {
			return err
		}
	}
	for _, dir := range []string{agyHome, agyHome + "/agy", agyHome + "/workspace"} {
		if err := h.command(ctx, "chown", "portal-agy:portal-agy", dir); err != nil {
			return err
		}
	}
	return nil
}

func (h hostInstaller) installOfficialAGY(ctx context.Context) error {
	target := "/opt/cliproxy-agy/bin/agy"
	if info, err := os.Lstat(h.path(target)); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 {
			return errors.New("现有 AGY 官方程序权限无效")
		}
		if trustedDeploymentFile(h.path(target)) != nil {
			return errors.New("现有 AGY 官方程序所有者无效")
		}
		return nil // Preserve the administrator's installed release on same-host restore.
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("AGY 官方程序无法检查")
	}
	release, ok := agyReleases[runtime.GOARCH]
	if !ok {
		return errors.New("当前架构不支持 AGY 自动重建")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client := downloadClient(&http.Client{Timeout: 5 * time.Minute}, true)
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) > 2 || r.URL.Scheme != "https" || r.URL.Hostname() != "storage.googleapis.com" {
			return errors.New("AGY 官方下载重定向无效")
		}
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", release.URL, nil)
	r, err := client.Do(req)
	if err != nil {
		return errors.New("AGY 官方程序下载失败")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return errors.New("AGY 官方程序下载未成功")
	}
	tmp, err := os.CreateTemp(h.path("/opt/cliproxy-agy/bin"), ".official-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha512.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(r.Body, 100<<20+1))
	if err != nil || n > 100<<20 {
		return errors.New("AGY 官方下载中断或过大")
	}
	if hex.EncodeToString(hash.Sum(nil)) != release.SHA512 {
		return errors.New("AGY 官方程序 SHA512 不匹配")
	}
	if _, err = tmp.Seek(0, 0); err != nil {
		return err
	}
	gz, err := gzip.NewReader(tmp)
	if err != nil {
		return errors.New("AGY 官方压缩包无效")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for {
		entry, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errors.New("AGY 官方压缩包损坏")
		}
		if entry.Name != "antigravity" || entry.Typeflag != tar.TypeReg || entry.Size <= 0 || entry.Size > 400<<20 || found {
			return errors.New("AGY 官方压缩包内容不符合预期")
		}
		f, e := os.CreateTemp(h.path("/opt/cliproxy-agy/bin"), ".official-*")
		if e != nil {
			return e
		}
		e = writeOfficialExecutable(tr, f, entry.Size, h.path(target))
		if e != nil {
			return e
		}
		found = true
	}
	if !found {
		return errors.New("AGY 官方压缩包缺少程序")
	}
	return nil
}

func writeOfficialExecutable(r io.Reader, f *os.File, size int64, target string) error {
	defer os.Remove(f.Name())
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if err != nil || n != size {
		return errors.New("AGY 官方程序解压失败")
	}
	if err = f.Chmod(0755); err == nil {
		err = f.Sync()
	}
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), target)
}
