package agy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client speaks only to the configured private worker. Caller credentials are
// replaced by service authentication and an independently authenticated owner.
type Client struct {
	ImageBaseURL string
	base, secret string
	http         *http.Client
	statusMu     sync.Mutex
	status       Status
	statusErr    error
	statusUntil  time.Time
	statusFlight chan struct{}
}

type progressContextKey struct{}

func WithModelProgress(ctx context.Context, progress func()) context.Context {
	return context.WithValue(ctx, progressContextKey{}, progress)
}

func NewClient(base, secret string) (*Client, error) {
	u, e := url.Parse(base)
	if e != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(secret) < 32 {
		return nil, errors.New("AGY 内部服务地址或凭据无效")
	}
	return &Client{base: strings.TrimRight(base, "/"), secret: secret, http: &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Status reads in-memory worker counters only. Coalesce page/poll requests and
// cache for one UI refresh interval; never discover models or open native DBs.
func (c *Client) Status(ctx context.Context) (Status, error) {
	for {
		c.statusMu.Lock()
		if time.Now().Before(c.statusUntil) {
			v, err := c.status, c.statusErr
			c.statusMu.Unlock()
			return v, err
		}
		if flight := c.statusFlight; flight != nil {
			c.statusMu.Unlock()
			select {
			case <-flight:
				continue
			case <-ctx.Done():
				return Status{}, ctx.Err()
			}
		}
		flight := make(chan struct{})
		c.statusFlight = flight
		c.statusMu.Unlock()
		readCtx, done := context.WithTimeout(ctx, time.Second)
		var v Status
		err := c.Read(readCtx, "/internal/status", &v)
		done()
		ttl := 5 * time.Second
		if err != nil {
			ttl = 2 * time.Second
		}
		c.statusMu.Lock()
		c.status, c.statusErr, c.statusUntil = v, err, time.Now().Add(ttl)
		c.statusFlight = nil
		close(flight)
		c.statusMu.Unlock()
		return v, err
	}
}
func (c *Client) Request(ctx context.Context, method, path, owner string, body io.Reader) (*http.Response, error) {
	if progress, ok := ctx.Value(progressContextKey{}).(func()); ok {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			if code == http.StatusProcessing {
				progress()
			}
			return nil
		}})
	}
	r, e := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if e != nil {
		return nil, e
	}
	r.Header.Set("Authorization", "Bearer "+c.secret)
	r.Header.Set("Content-Type", "application/json")
	if path == "/internal/responses" {
		if base := publicImageBase(c.ImageBaseURL); base != "" {
			r.Header.Set("X-Portal-Image-Base-URL", base)
		}
	}
	if owner != "" {
		r.Header.Set("X-Portal-Owner", owner)
	}
	return c.http.Do(r)
}
func (c *Client) Read(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, e := c.Request(ctx, "GET", path, "", nil)
	if e != nil {
		return fail(503, "worker_unavailable", "AGY 内部服务暂不可用")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fail(503, "worker_unavailable", "AGY 尚未连接或尚未登录")
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}
func authorized(r *http.Request, secret string) bool {
	v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return len(v) == len(secret) && subtle.ConstantTimeCompare([]byte(v), []byte(secret)) == 1
}
func WriteError(w http.ResponseWriter, err error) {
	e := &Error{502, "agent_error", "AGY 请求失败"}
	var v *Error
	if errors.As(err, &v) {
		e = v
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "agy_error", "code": e.Code, "message": e.Message}})
}
