package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

func (h hostInstaller) installCloudflared(ctx context.Context) error {
	if info, err := os.Stat(h.path("/usr/local/bin/cloudflared")); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return nil
	}
	if info, err := os.Stat(h.path("/usr/bin/cloudflared")); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		data, err := os.ReadFile(h.path("/usr/bin/cloudflared"))
		if err != nil {
			return errors.New("已安装的 cloudflared 无法读取")
		}
		return h.write("/usr/local/bin/cloudflared", data, 0o755, true)
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return errors.New("当前架构不支持自动安装 cloudflared")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client := downloadClient(&http.Client{Timeout: 5 * time.Minute}, true)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 4 || req.URL.Scheme != "https" || (req.URL.Hostname() != "github.com" && req.URL.Hostname() != "release-assets.githubusercontent.com" && req.URL.Hostname() != "objects.githubusercontent.com") {
			return errors.New("安装下载地址不受支持")
		}
		return nil
	}
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/cloudflare/cloudflared/releases/latest", nil)
	request.Header.Set("User-Agent", "CLIProxy-Portal-Installer")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("无法连接官方 cloudflared 版本接口")
	}
	defer response.Body.Close()
	var release struct {
		Assets []struct {
			Name, Digest string
			URL          string `json:"browser_download_url"`
		}
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release) != nil {
		return errors.New("官方 cloudflared 版本信息无法校验")
	}
	for _, asset := range release.Assets {
		if asset.Name != "cloudflared-linux-"+runtime.GOARCH {
			continue
		}
		digest := strings.TrimPrefix(asset.Digest, "sha256:")
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || !strings.HasPrefix(asset.Digest, "sha256:") || !strings.HasPrefix(asset.URL, "https://github.com/cloudflare/cloudflared/releases/download/") {
			return errors.New("官方 cloudflared 附件缺少有效 SHA256，停止自动安装")
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", asset.URL, nil)
		r, err := client.Do(req)
		if err != nil {
			return errors.New("cloudflared 官方附件下载失败")
		}
		defer r.Body.Close()
		data, err := io.ReadAll(io.LimitReader(r.Body, 100<<20+1))
		if err != nil || r.StatusCode != 200 || len(data) > 100<<20 {
			return errors.New("cloudflared 下载中断或超过大小限制")
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != digest {
			return errors.New("cloudflared 下载 SHA256 不一致")
		}
		return h.write("/usr/local/bin/cloudflared", data, 0o755, true)
	}
	return errors.New("官方版本缺少当前架构的 cloudflared 附件")
}
