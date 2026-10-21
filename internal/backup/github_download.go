package backup

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const downloadAttempts = 3
const downloadIdleTimeout = 30 * time.Second

var errDownloadIdle = errors.New("download idle timeout")

type downloadProgress struct {
	Step, Reason string
	Attempt      int
	Bytes, Total int64
}

type downloadFailure struct {
	step, reason string
	retry        bool
}

func (e *downloadFailure) Error() string {
	return "远端备份下载校验失败（" + e.step + "）：" + e.reason
}

// Verification GETs have their own connection pool. A failed or stale upload/API
// connection must not stall verification for the upload client's ten minutes.
// Retries get fresh signed URLs and connections; only GETs are retried.
func downloadClient(base *http.Client, fallback bool) *http.Client {
	c := *base
	var transport *http.Transport
	switch t := base.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = t.Clone()
	}
	if transport != nil {
		transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		transport.TLSHandshakeTimeout = 10 * time.Second
		transport.ResponseHeaderTimeout = 20 * time.Second
		transport.IdleConnTimeout = 10 * time.Second
		if fallback {
			transport.ForceAttemptHTTP2 = false
			transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{}
			}
			transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
			transport.Protocols = new(http.Protocols)
			transport.Protocols.SetHTTP1(true)
		}
		c.Transport = transport
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func downloadNetworkFailure(ctx context.Context, step string, err error) *downloadFailure {
	if errors.Is(context.Cause(ctx), errDownloadIdle) {
		return &downloadFailure{step, "连续 30 秒未收到数据", true}
	}
	var dns *net.DNSError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate x509.CertificateInvalidError
	var network net.Error
	switch {
	case errors.As(err, &authority), errors.As(err, &hostname), errors.As(err, &certificate):
		return &downloadFailure{step, "TLS 证书校验失败", false}
	case errors.As(err, &dns):
		return &downloadFailure{step, "DNS 解析失败", true}
	case errors.As(err, &network) && network.Timeout():
		return &downloadFailure{step, "连接或等待响应超时", true}
	default:
		// Never include err.Error(): URL errors can contain signed URLs or tokens.
		return &downloadFailure{step, "网络连接中断或响应读取失败", true}
	}
}

func (g *GitHub) verifyAsset(ctx context.Context, repository string, id int64, expected Entry) error {
	return g.verifyAssetProgress(ctx, repository, id, expected, nil)
}

func (g *GitHub) verifyAssetProgress(ctx context.Context, repository string, id int64, expected Entry, stages *stageHooks) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		err := g.verifyAssetAttempt(ctx, repository, id, expected, attempt, stages)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.New("远端备份下载校验已取消或超过总时限")
		}
		var failure *downloadFailure
		if !errors.As(err, &failure) || !failure.retry || attempt == downloadAttempts {
			return fmt.Errorf("%w（已尝试 %d 次；旧备份不会清理）", err, attempt)
		}
		stages.download(downloadProgress{Step: "retry", Reason: failure.Error(), Attempt: attempt, Total: expected.Size})
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("远端备份下载校验已取消")
		case <-timer.C:
		}
	}
	return errors.New("远端备份下载校验失败")
}

func (g *GitHub) verifyAssetAttempt(parent context.Context, repository string, id int64, expected Entry, attempt int, stages *stageHooks) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	idleTimeout := g.downloadIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = downloadIdleTimeout
	}
	idle := time.AfterFunc(idleTimeout, func() { cancel(errDownloadIdle) })
	defer idle.Stop()
	client := downloadClient(g.client, attempt > 1)
	defer client.CloseIdleConnections()
	stages.download(downloadProgress{Step: "address", Attempt: attempt, Total: expected.Size})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+"/repos/"+repository+"/releases/assets/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return &downloadFailure{"获取附件地址", "GitHub 附件地址无效", false}
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "CLIProxy-Portal-Backup")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return downloadNetworkFailure(ctx, "获取附件地址", err)
	}
	if resp.StatusCode == http.StatusFound {
		location, parseErr := url.Parse(resp.Header.Get("Location"))
		_ = resp.Body.Close()
		if parseErr != nil || location.Scheme != "https" || location.User != nil || location.Port() != "" || !(location.Hostname() == "release-assets.githubusercontent.com" || location.Hostname() == "objects.githubusercontent.com") {
			return &downloadFailure{"获取附件地址", "GitHub 附件下载跳转无效", false}
		}
		// Storage receives no repository token, including on retries/redirects.
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
		if err != nil {
			return &downloadFailure{"连接附件存储节点", "GitHub 附件下载链接无效", false}
		}
		req.Header.Set("User-Agent", "CLIProxy-Portal-Backup")
		stages.download(downloadProgress{Step: "connect", Attempt: attempt, Total: expected.Size})
		idle.Reset(idleTimeout)
		resp, err = client.Do(req)
		if err != nil {
			return downloadNetworkFailure(ctx, "连接附件存储节点", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		status := resp.StatusCode
		return &downloadFailure{"下载附件", fmt.Sprintf("HTTP %d", status), status == 408 || status == 429 || status >= 500 || (status == 403 && req.Header.Get("Authorization") == "")}
	}
	h := sha256.New()
	reader := io.LimitReader(resp.Body, expected.Size+1)
	buffer := make([]byte, 64*1024)
	var received int64
	last := time.Time{}
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			idle.Reset(idleTimeout)
			h.Write(buffer[:n])
			received += int64(n)
			if time.Since(last) >= 2*time.Second || received >= expected.Size {
				stages.download(downloadProgress{Step: "transfer", Attempt: attempt, Bytes: received, Total: expected.Size})
				last = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return downloadNetworkFailure(ctx, "读取附件数据", readErr)
		}
	}
	if received != expected.Size || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
		return &downloadFailure{"核对附件内容", "远端备份大小或 SHA256 不一致", false}
	}
	return nil
}
