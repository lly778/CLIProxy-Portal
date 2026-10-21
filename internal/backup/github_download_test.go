package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type downloadRoundTrip func(*http.Request) (*http.Response, error)

func (f downloadRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func downloadEntry(body string) Entry {
	h := sha256.Sum256([]byte(body))
	return Entry{Size: int64(len(body)), SHA256: hex.EncodeToString(h[:])}
}

func TestDownloadRetriesTransientStatusOnFreshHTTP1Connection(t *testing.T) {
	const body = "encrypted-backup-bytes"
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-only-token" || r.UserAgent() != "CLIProxy-Portal-Backup" {
			t.Error("missing download headers")
		}
		if n == 1 {
			if r.ProtoMajor != 2 {
				t.Errorf("initial request was not HTTP/2: %s", r.Proto)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.ProtoMajor != 1 {
			t.Errorf("fallback was not HTTP/1.1: %s", r.Proto)
		}
		io.WriteString(w, body)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	g := NewGitHub("test-only-token")
	g.api, g.client = server.URL, server.Client()
	g.client.Transport.(*http.Transport).ForceAttemptHTTP2 = true
	var progress []downloadProgress
	err := g.verifyAssetProgress(t.Context(), "owner/private", 1, downloadEntry(body), &stageHooks{progress: func(p downloadProgress) { progress = append(progress, p) }})
	if err != nil || requests.Load() != 2 {
		t.Fatalf("retry failed: %v requests=%d", err, requests.Load())
	}
	var retry, complete bool
	for _, p := range progress {
		if p.Step == "retry" && p.Attempt == 1 {
			retry = true
		}
		if p.Step == "transfer" && p.Attempt == 2 && p.Bytes == int64(len(body)) {
			complete = true
		}
	}
	if !retry || !complete {
		t.Fatalf("missing retry or byte progress: %+v", progress)
	}
}

func TestDownloadRecoversFromHeaderAndBodyStalls(t *testing.T) {
	for _, stall := range []string{"headers", "body"} {
		t.Run(stall, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					if stall == "body" {
						w.WriteHeader(200)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return
				}
				io.WriteString(w, "verified")
			}))
			defer server.Close()
			g := NewGitHub("test-only-token")
			g.api, g.client = server.URL, server.Client()
			g.downloadIdleTimeout = 50 * time.Millisecond
			if stall == "headers" {
				g.client.Timeout = 40 * time.Millisecond
			}
			if err := g.verifyAsset(t.Context(), "owner/private", 1, downloadEntry("verified")); err != nil || requests.Load() != 2 {
				t.Fatalf("stalled %s did not recover: %v (%d)", stall, err, requests.Load())
			}
		})
	}
}

func TestDownloadRedirectNeverLeaksTokenOrSignedURL(t *testing.T) {
	for _, mode := range []string{"success", "network-error", "second-redirect"} {
		t.Run(mode, func(t *testing.T) {
			g := NewGitHub("test-only-token")
			var storageCalls int
			g.client = &http.Client{Transport: downloadRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.github.com" {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://release-assets.githubusercontent.com/asset?signature=private-signed-secret"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				storageCalls++
				if r.URL.Host != "release-assets.githubusercontent.com" || r.Header.Get("Authorization") != "" || r.UserAgent() != "CLIProxy-Portal-Backup" {
					t.Fatal("storage redirect leaked credentials or followed an unsafe host")
				}
				if mode == "network-error" {
					return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: context.DeadlineExceeded}
				}
				if mode == "second-redirect" {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://attacker.example/steal"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("verified"))}, nil
			})}
			err := g.verifyAsset(t.Context(), "owner/private", 1, downloadEntry("verified"))
			if mode == "success" {
				if err != nil || storageCalls != 1 {
					t.Fatalf("download: %v calls=%d", err, storageCalls)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-signed-secret") || strings.Contains(err.Error(), g.token) {
				t.Fatalf("unsafe download error: %v", err)
			}
		})
	}
}

func TestDownloadIntegrityAndCancellationNeverRetryOrPublish(t *testing.T) {
	for _, mode := range []string{"corrupt", "forbidden", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			g := NewGitHub("test-only-token")
			var calls int
			g.client = &http.Client{Transport: downloadRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Context().Err() != nil {
					return nil, r.Context().Err()
				}
				status := 200
				if mode == "forbidden" {
					status = 403
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("corrupt!"))}, nil
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			err := g.verifyAsset(ctx, "owner/private", 1, downloadEntry("verified"))
			if err == nil || calls > 1 {
				t.Fatalf("permanent failure retried: %v calls=%d", err, calls)
			}
		})
	}
}

func TestDownloadErrorClassifiesIdleWithoutRawNetworkDetails(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errDownloadIdle)
	e := downloadNetworkFailure(ctx, "读取附件数据", errors.New("private-signed-url"))
	if !e.retry || !strings.Contains(e.Error(), "连续 30 秒") || strings.Contains(e.Error(), "private-signed-url") {
		t.Fatalf("unsafe error: %v", e)
	}
}

func TestGitHubAPIReadsRetryButWritesNeverRepeat(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			g := NewGitHub("test-only-token")
			var calls int
			g.client.Transport = downloadRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, &url.Error{URL: "https://private-signed-secret", Err: context.DeadlineExceeded}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			resp, err := g.request(t.Context(), method, g.api+"/repos/owner/private", "application/json", strings.NewReader("{}"), 2)
			if method == http.MethodGet {
				if err != nil || calls != 2 {
					t.Fatalf("safe GET not recovered: %v calls=%d", err, calls)
				}
				resp.Body.Close()
			} else if err == nil || calls != 1 || strings.Contains(err.Error(), "private-signed-secret") {
				t.Fatalf("write retried or leaked request: %v calls=%d", err, calls)
			}
		})
	}
}

func TestGitHubAPIRequestsUseFreshConnections(t *testing.T) {
	var addresses []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		addresses = append(addresses, r.RemoteAddr)
		mu.Unlock()
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	g := NewGitHub("test-only-token")
	g.api = server.URL
	for i := 0; i < 2; i++ {
		var value map[string]any
		if err := g.json(t.Context(), http.MethodGet, "/repos/owner/private", nil, &value, 200); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(addresses) != 2 || addresses[0] == addresses[1] {
		t.Fatalf("API reused idle connection: %v", addresses)
	}
}
