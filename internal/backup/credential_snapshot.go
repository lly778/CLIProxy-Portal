package backup

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const credentialsSnapshotName = "portal/backup/credentials.json"

type esxiCredentials struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Fingerprint string `json:"certificate_sha256"`
}

type credentialSnapshot struct {
	Version         int              `json:"version"`
	Repository      string           `json:"repository"`
	GitHubToken     string           `json:"github_token,omitempty"`
	ESXi            *esxiCredentials `json:"esxi,omitempty"`
	CloudflareToken string           `json:"cloudflare_token,omitempty"`
}

var tunnelTokenValue = regexp.MustCompile(`^[A-Za-z0-9._/+=-]+$`)
var inlineTunnelToken = regexp.MustCompile(`--token(?:=|[ \t]+)["']?([A-Za-z0-9._/+=-]+)`)

func (v credentialSnapshot) validate() error {
	if v.Version != 1 || !repositoryPattern.MatchString(v.Repository) || ValidateToken(v.GitHubToken) != nil {
		return errors.New("备份 GitHub 凭据格式无效")
	}
	if v.ESXi != nil {
		e := v.ESXi
		u, err := url.Parse(e.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			e.Username == "" || len(e.Username) > 1024 || e.Password == "" || len(e.Password) > 4096 ||
			!regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(strings.ReplaceAll(e.Fingerprint, ":", "")) {
			return errors.New("备份 ESXi 凭据或证书指纹格式无效")
		}
	}
	if v.CloudflareToken != "" && (len(v.CloudflareToken) < 32 || len(v.CloudflareToken) > 16384 || !tunnelTokenValue.MatchString(v.CloudflareToken)) {
		return errors.New("备份隧道凭据格式无效")
	}
	return nil
}

// Values are serialized only into a root-private job file and then encrypted.
// Never print this structure, raw parsing errors, or secret-bearing unit lines.
func snapshotCredentials(dir, work, repository, hostEtc string) (Source, error) {
	token, err := LoadToken(dir)
	if err != nil {
		return Source{}, errors.New("GitHub 凭据无法读取")
	}
	v := credentialSnapshot{Version: 1, Repository: repository, GitHubToken: token}
	if hostEtc == "" {
		return writeCredentialSnapshot(v, work)
	}
	esxiFile := filepath.Join(hostEtc, "cliproxy-esxi/esxi.json")
	if _, err := os.Lstat(esxiFile); err == nil {
		if trustedHostFile(esxiFile) != nil {
			return Source{}, errors.New("ESXi 凭据文件权限不安全")
		}
		var e esxiCredentials
		if readDeploymentSnapshot(esxiFile, &e) != nil {
			return Source{}, errors.New("ESXi 凭据文件无法读取")
		}
		v.ESXi = &e
	} else if !errors.Is(err, os.ErrNotExist) {
		return Source{}, errors.New("ESXi 凭据文件无法检查")
	}
	tunnelFile := filepath.Join(hostEtc, "cliproxy-cloudflared/tunnel-token")
	if _, err := os.Lstat(tunnelFile); err == nil {
		if trustedHostFile(tunnelFile) != nil {
			return Source{}, errors.New("隧道凭据文件权限不安全")
		}
		data, err := os.ReadFile(tunnelFile)
		if err != nil || len(data) > 16385 {
			return Source{}, errors.New("隧道凭据文件无法读取")
		}
		v.CloudflareToken = strings.TrimSpace(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Source{}, errors.New("隧道凭据文件无法检查")
	}
	if v.CloudflareToken == "" {
		for _, unit := range []string{"cliproxy-portal-tunnel.service", "cloudflared.service", "cloudflared-cliproxy.service"} {
			file := filepath.Join(hostEtc, "systemd/system", unit)
			if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
				continue
			}
			if trustedDeploymentFile(file) != nil {
				return Source{}, errors.New("隧道服务定义权限不安全")
			}
			data, err := os.ReadFile(file)
			if err != nil || len(data) > 1<<20 {
				return Source{}, errors.New("隧道服务定义无法读取")
			}
			if match := inlineTunnelToken.FindSubmatch(data); len(match) == 2 {
				v.CloudflareToken = string(match[1])
				break
			}
		}
	}
	return writeCredentialSnapshot(v, work)
}

func writeCredentialSnapshot(v credentialSnapshot, work string) (Source, error) {
	if err := v.validate(); err != nil {
		return Source{}, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return Source{}, errors.New("凭据快照无法生成")
	}
	file := filepath.Join(work, "backup-credentials.json")
	if err := atomicWrite(file, data, 0o600); err != nil {
		return Source{}, errors.New("凭据快照无法写入")
	}
	return Source{Path: file, Name: credentialsSnapshotName, Required: true}, nil
}

func readCredentials(restored string) (*credentialSnapshot, error) {
	var v credentialSnapshot
	err := readDeploymentSnapshot(filepath.Join(restored, filepath.FromSlash(credentialsSnapshotName)), &v)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || v.validate() != nil {
		return nil, errors.New("加密备份中的凭据快照无效")
	}
	return &v, nil
}
