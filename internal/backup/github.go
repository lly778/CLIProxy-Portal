package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type GitHub struct {
	client  *http.Client
	api     string
	uploads string
	token   string
}

type release struct {
	ID     int64   `json:"id"`
	Tag    string  `json:"tag_name"`
	Body   string  `json:"body"`
	Draft  bool    `json:"draft"`
	URL    string  `json:"html_url"`
	Assets []asset `json:"assets"`
}

type asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Digest string `json:"digest"`
}

func NewGitHub(token string) *GitHub {
	return &GitHub{client: &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, api: "https://api.github.com", uploads: "https://uploads.github.com", token: token}
}

func (g *GitHub) request(ctx context.Context, method, target, contentType string, body io.Reader, length int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, errors.New("GitHub 请求无效")
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "CLIProxy-Portal-Backup")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 GitHub，请检查网络或稍后重试")
	}
	return resp, nil
}

func (g *GitHub) json(ctx context.Context, method, endpoint string, payload, result any, status int) error {
	var b []byte
	var err error
	if payload != nil {
		b, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	resp, err := g.request(ctx, method, g.api+endpoint, "application/json", bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		return fmt.Errorf("GitHub 操作失败（HTTP %d），请检查仓库初始化、令牌权限及有效期", resp.StatusCode)
	}
	if result != nil {
		if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(result); err != nil {
			return errors.New("GitHub 返回数据无效")
		}
	}
	return nil
}

func (g *GitHub) CheckPrivate(ctx context.Context, repository string) error {
	if !repositoryPattern.MatchString(repository) {
		return errors.New("仓库格式无效")
	}
	var repo struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Archived bool   `json:"archived"`
		Disabled bool   `json:"disabled"`
	}
	if err := g.json(ctx, http.MethodGet, "/repos/"+repository, nil, &repo, http.StatusOK); err != nil {
		return err
	}
	if !strings.EqualFold(repo.FullName, repository) || !repo.Private || repo.Archived || repo.Disabled {
		return errors.New("只允许上传到可用的 GitHub 私有仓库；公开、归档或已转移的仓库会被拒绝")
	}
	return nil
}

func (g *GitHub) Upload(ctx context.Context, c Config, file, tag string) (string, int64, string, error) {
	return g.upload(ctx, c, file, tag, nil)
}

func (g *GitHub) upload(ctx context.Context, c Config, file, tag string, stages *stageHooks) (string, int64, string, error) {
	finishUpload := stages.start("upload")
	defer finishUpload()
	if err := g.CheckPrivate(ctx, c.Repository); err != nil {
		return "", 0, "", err
	}
	entry, err := describe(file, archiveName, false)
	if err != nil {
		return "", 0, "", err
	}
	if entry.Size >= 2<<30 {
		return "", 0, "", errors.New("备份文件超过 GitHub Release 单文件限制")
	}
	var r release
	body := releaseMarker + "\nInstance: " + c.Instance + "\nSHA256: " + entry.SHA256 + "\nEncrypted bytes: " + strconv.FormatInt(entry.Size, 10)
	err = g.json(ctx, http.MethodPost, "/repos/"+c.Repository+"/releases", map[string]any{"tag_name": tag, "name": "门户备份 " + time.Now().UTC().Format(time.RFC3339), "body": body, "draft": true, "make_latest": "false"}, &r, http.StatusCreated)
	if err != nil {
		return "", 0, "", err
	}
	if r.ID <= 0 || r.Tag != tag {
		return "", 0, "", errors.New("GitHub 备份发布返回数据无效")
	}
	// Repository privacy is rechecked immediately before sending encrypted data.
	if err = g.CheckPrivate(ctx, c.Repository); err != nil {
		return "", 0, "", err
	}
	f, err := os.Open(file)
	if err != nil {
		return "", 0, "", err
	}
	var uploaded asset
	uploaded, err = g.uploadAsset(ctx, c.Repository, r.ID, archiveName, f, entry.Size)
	_ = f.Close()
	if err != nil {
		return "", 0, "", err
	}
	if uploaded.ID <= 0 || uploaded.Name != archiveName || uploaded.Size != entry.Size || uploaded.State != "uploaded" {
		return "", 0, "", errors.New("GitHub 附件大小或上传状态不一致")
	}
	if uploaded.Digest != "" && uploaded.Digest != "sha256:"+entry.SHA256 {
		return "", 0, "", errors.New("GitHub 附件校验不一致")
	}
	finishUpload()
	finishDownload := stages.start("download_verify")
	defer finishDownload()
	// Download and hash the encrypted asset, not just its upload response.
	if err = g.verifyAsset(ctx, c.Repository, uploaded.ID, entry); err != nil {
		return "", 0, "", err
	}
	finishDownload()
	finishPublish := stages.start("publish")
	defer finishPublish()
	if err = g.CheckPrivate(ctx, c.Repository); err != nil {
		return "", 0, "", err
	}
	if err = g.json(ctx, http.MethodPatch, "/repos/"+c.Repository+"/releases/"+strconv.FormatInt(r.ID, 10), map[string]any{"draft": false, "make_latest": "false"}, &r, http.StatusOK); err != nil {
		return "", 0, "", err
	}
	expectedPrefix := "https://github.com/" + c.Repository + "/releases/"
	if !strings.HasPrefix(r.URL, expectedPrefix) {
		return "", 0, "", errors.New("GitHub 发布链接无效")
	}
	return r.URL, entry.Size, entry.SHA256, nil
}

func (g *GitHub) uploadAsset(ctx context.Context, repository string, releaseID int64, name string, body io.Reader, size int64) (asset, error) {
	endpoint := g.uploads + "/repos/" + repository + "/releases/" + strconv.FormatInt(releaseID, 10) + "/assets?name=" + url.QueryEscape(name)
	resp, err := g.request(ctx, http.MethodPost, endpoint, "application/octet-stream", body, size)
	if err != nil {
		return asset{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return asset{}, fmt.Errorf("GitHub 附件上传失败（HTTP %d）", resp.StatusCode)
	}
	var a asset
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		return a, errors.New("GitHub 附件返回数据无效")
	}
	return a, nil
}

func (g *GitHub) verifyAsset(ctx context.Context, repository string, id int64, expected Entry) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+"/repos/"+repository+"/releases/assets/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client.Do(req)
	if err != nil {
		return errors.New("下载备份附件进行校验失败")
	}
	if resp.StatusCode == http.StatusFound {
		location, e := url.Parse(resp.Header.Get("Location"))
		_ = resp.Body.Close()
		if e != nil || location.Scheme != "https" || location.User != nil || location.Port() != "" || !(location.Hostname() == "release-assets.githubusercontent.com" || location.Hostname() == "objects.githubusercontent.com") {
			return errors.New("GitHub 附件下载跳转无效")
		}
		// Signed storage URLs must never receive the repository token.
		req, e = http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
		if e != nil {
			return errors.New("GitHub 附件下载链接无效")
		}
		resp, err = g.client.Do(req)
		if err != nil {
			return errors.New("下载备份附件进行校验失败")
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("备份下载校验失败（HTTP %d）", resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, expected.Size+1))
	if err != nil || n != expected.Size || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
		return errors.New("远端备份大小或 SHA256 校验失败，旧备份不会清理")
	}
	return nil
}

// Retain only touches complete releases belonging to this installation. It is
// called after a new backup is downloaded and verified; unrelated releases stay.
func (g *GitHub) Retain(ctx context.Context, c Config, newestTag string) error {
	if c.Retain < 1 || c.Retain > 30 || !instancePattern.MatchString(c.Instance) {
		return errors.New("旧备份保留设置无效")
	}
	if err := g.CheckPrivate(ctx, c.Repository); err != nil {
		return err
	}
	pattern := regexp.MustCompile(`^(?:` + regexp.QuoteMeta(releaseTagPrefix) + `|` + regexp.QuoteMeta(legacyReleaseTagPrefix) + `)` + regexp.QuoteMeta(c.Instance) + `-([0-9]{8}T[0-9]{6}Z)-[a-f0-9]{8}$`)
	if !pattern.MatchString(newestTag) {
		return errors.New("当前备份标识无效，已跳过旧备份清理")
	}
	var ours []release
	timestamps := map[string]string{}
	newestFound := false
	completeListing := false
	for page := 1; page <= 20; page++ {
		var rows []release
		if err := g.json(ctx, http.MethodGet, "/repos/"+c.Repository+"/releases?per_page=100&page="+strconv.Itoa(page), nil, &rows, http.StatusOK); err != nil {
			return err
		}
		for _, r := range rows {
			match := pattern.FindStringSubmatch(r.Tag)
			marked := strings.HasPrefix(r.Body, releaseMarker+"\nInstance: "+c.Instance+"\n") || strings.HasPrefix(r.Body, legacyReleaseMarker+"\nInstance: "+c.Instance+"\n")
			if r.ID <= 0 || r.Draft || len(match) != 2 || !marked {
				continue
			}
			complete := false
			for _, a := range r.Assets {
				if (a.Name == archiveName || a.Name == legacyArchiveName) && a.State == "uploaded" && a.Size > 0 {
					complete = true
				}
			}
			if complete {
				ours = append(ours, r)
				timestamps[r.Tag] = match[1]
				newestFound = newestFound || r.Tag == newestTag
			}
		}
		if len(rows) < 100 {
			completeListing = true
			break
		}
	}
	if !completeListing {
		return errors.New("发布记录过多，已跳过旧备份清理")
	}
	if !newestFound {
		return errors.New("未找到已发布的当前备份，已跳过旧备份清理")
	}
	sort.Slice(ours, func(i, j int) bool {
		if ours[i].Tag == ours[j].Tag {
			return false
		}
		if ours[i].Tag == newestTag {
			return true
		}
		if ours[j].Tag == newestTag {
			return false
		}
		if timestamps[ours[i].Tag] != timestamps[ours[j].Tag] {
			return timestamps[ours[i].Tag] > timestamps[ours[j].Tag]
		}
		return ours[i].Tag > ours[j].Tag
	})
	for i := c.Retain; i < len(ours); i++ {
		r := ours[i]
		if err := g.json(ctx, http.MethodDelete, "/repos/"+c.Repository+"/releases/"+strconv.FormatInt(r.ID, 10), nil, nil, http.StatusNoContent); err != nil {
			return err
		}
		if err := g.json(ctx, http.MethodDelete, "/repos/"+c.Repository+"/git/refs/tags/"+r.Tag, nil, nil, http.StatusNoContent); err != nil {
			return err
		}
	}
	return nil
}
