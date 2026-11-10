package backup

import (
	"archive/tar"
	portalops "cliproxy-portal/ops"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"
)

// Fixed official release, checksums from Google's platform updater manifests.
// No download address or executable is ever taken from an uploaded archive.
var agyReleases = map[string]struct{ URL, SHA512 string }{
	"amd64": {"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.4-5037094733742080/linux-x64/cli_linux_x64.tar.gz", "1431a409ef7c7bce23f4abf03f988afab728ba87d91d24ed7db0d5eea3ab71a4b590bf0bf2c79cec84f79b32b674ea38df0417356ce8d5d4f698cd62ea9ee6be"},
	"arm64": {"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.4-5037094733742080/linux-arm/cli_linux_arm64.tar.gz", "564b9e6841ab4631e370d674c52f6eac75fa066fd6f61b1af39cb186dc57d835921d68e598293c7e5237d5605b90c0d3977805c7890f99dd8d9cc2e7471d8ff8"},
}

// The bridge runs inside the portal; only external CLI/data mounts are prepared.
func (h hostInstaller) installAGY(ctx context.Context) error {
	var c struct {
		MaxSessions int  `json:"max_sessions"`
		MemoryMiB   int  `json:"memory_mib"`
		LoggedIn    bool `json:"logged_in"`
	}
	if readDeploymentSnapshot(h.path(agyConfigPath), &c) != nil || c.MaxSessions < 1 || c.MaxSessions > 16 || c.MemoryMiB < 128 || c.MemoryMiB > 4096 {
		return errors.New("AGY 恢复配置无效")
	}
	if _, err := privateAGYFile(h.path(agySecretPath), true); err != nil {
		return err
	}
	if err := h.prepareAGYHost(ctx); err != nil {
		return err
	}
	var err error
	if h.agyDownload != nil {
		err = h.agyDownload(ctx)
	} else {
		err = h.installOfficialAGY(ctx)
	}
	if err != nil {
		return err
	}
	if !c.LoggedIn {
		if err = os.Remove(h.path(agyTokenPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if _, err = privateAGYFile(h.path(agyTokenPath), true); err != nil {
		return err
	}
	cache := h.path(agyHome + "/portal-sessions.db")
	if info, e := os.Lstat(cache); e == nil {
		if !info.Mode().IsRegular() {
			return errors.New("AGY 会话索引不是普通文件")
		}
		db, e := sql.Open("sqlite", cache)
		if e != nil {
			return e
		}
		_, e = db.ExecContext(ctx, `BEGIN IMMEDIATE; UPDATE sessions SET ready=0; DELETE FROM responses; COMMIT;`)
		_ = db.Close()
		if e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if err = h.command(ctx, "chown", "-R", "--no-dereference", "10001:10001", agyHome); err != nil {
		return err
	}
	if err = h.command(ctx, "chown", "10001:10001", defaultPortalRoot+"/secrets/portal_agy_secret"); err != nil {
		return err
	}
	for _, dir := range []string{"/opt/cliproxy-agy", "/opt/cliproxy-agy/bin"} {
		if err = os.Chmod(h.path(dir), 0755); err != nil {
			return err
		}
	}
	data := fmt.Sprintf("PORTAL_AGY_EMBEDDED=true\nAGY_MAX_SESSIONS=%d\nAGY_CONTAINER_MEMORY_MIB=%d\n", c.MaxSessions, c.MemoryMiB)
	if err = h.write("/etc/cliproxy-agy/runtime.env", []byte(data), 0600, false); err != nil {
		return err
	}
	dockerfile, e := portalops.AGYAssets.ReadFile("agy/Dockerfile.runtime")
	if e != nil {
		return e
	}
	return h.write(defaultPortalRoot+"/ops/agy/Dockerfile.runtime", dockerfile, 0644, false)
}
func (h hostInstaller) prepareAGYHost(ctx context.Context) error {
	for _, dir := range []string{"/etc/cliproxy-agy", "/opt/cliproxy-agy/bin", agyHome, agyHome + "/agy", agyHome + "/workspace"} {
		if err := h.directory(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}
func (h hostInstaller) agyContainerLimits() (int, int, error) {
	var c struct {
		MaxSessions int `json:"max_sessions"`
		MemoryMiB   int `json:"memory_mib"`
	}
	if _, err := os.Lstat(h.path(agyConfigPath)); errors.Is(err, os.ErrNotExist) {
		return 3, 1024, nil
	}
	if readDeploymentSnapshot(h.path(agyConfigPath), &c) != nil || c.MaxSessions < 1 || c.MaxSessions > 16 || c.MemoryMiB < 128 || c.MemoryMiB > 4096 {
		return 0, 0, errors.New("AGY 容器资源配置无效")
	}
	return c.MaxSessions, c.MemoryMiB, nil
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
