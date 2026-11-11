package gateway

import (
	"bufio"
	"bytes"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResourceWebsocketUpgradeHoldsCapacityAndPreservesNegotiation(t *testing.T) {
	upClosed := make(chan struct{})
	g, _ := resourceGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-WebSocket-Extensions") != "permessage-deflate" {
			t.Error("resource admission changed WebSocket negotiation")
		}
		c, b, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		defer c.Close()
		defer close(upClosed)
		_, _ = b.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = b.Flush()
		_, _ = io.Copy(io.Discard, b)
	})
	proxy := httptest.NewServer(g.Handler())
	defer proxy.Close()
	c, e := net.DialTimeout("tcp", strings.TrimPrefix(proxy.URL, "http://"), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, e = io.WriteString(c, "GET /v1/responses HTTP/1.1\r\nHost: test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
	if e != nil {
		t.Fatal(e)
	}
	reader := bufio.NewReader(c)
	response, e := http.ReadResponse(reader, nil)
	if e != nil || response.StatusCode != 101 {
		t.Fatal("upgrade failed", response, e)
	}
	if q := g.Resources.Snapshot(); q.Active != 1 {
		t.Fatal("upgraded connection lost admission", q)
	}
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}")))
	if w.Code != 429 {
		t.Fatal("WebSocket did not hold user quota", w.Code)
	}
	_ = c.Close()
	select {
	case <-upClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("closed upgraded connection stayed open")
	}
	deadline := time.Now().Add(time.Second)
	for g.Resources.Snapshot().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	q := g.Resources.Snapshot()
	if q.Active != 0 || q.Processing != 0 || q.Rejected != 1 {
		t.Fatal("upgraded connection leaked capacity or rejection event", q)
	}
}

func resourceGateway(t *testing.T, up http.HandlerFunc) (*Gateway, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); up(w, r) }))
	t.Cleanup(server.Close)
	s, e := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	v, e := NewVault(t.TempDir(), bytes.Repeat([]byte{3}, 32))
	if e != nil {
		t.Fatal(e)
	}
	g, e := New(server.URL, service.NewKeys(s, &gatewayCPAMP{}), s, v, nil)
	if e != nil {
		t.Fatal(e)
	}
	p := resourcecontrol.DefaultPolicy()
	p.Mode = "enforce"
	g.Resources = resourcecontrol.NewController(p)
	g.Resources.UpdateMetrics(resourcecontrol.Metrics{At: time.Now(), HostID: "test", CPUCount: 1, TotalMiB: 926, AvailableMiB: 400}, time.Now())
	return g, calls
}
func TestResourceConcurrencyHoldsStreamsAndReleasesAfterCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	g, calls := resourceGateway(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`)).WithContext(ctx))
	}()
	<-entered
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`)))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || calls.Load() != 1 {
		t.Fatal("concurrent stream admitted", w.Code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not release stream")
	}
	if q := g.Resources.Snapshot(); q.Active != 0 || q.Processing != 0 {
		t.Fatal("cancellation leaked capacity", q)
	}
}
func TestResourceObservePreservesRequestBytesAndProtectedWireBytes(t *testing.T) {
	body := []byte(`{"model":"x","input":"ok"}`)
	g, _ := resourceGateway(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, body) {
			t.Errorf("body changed: %q", b)
		}
		w.WriteHeader(200)
	})
	for _, mode := range []string{"observe", "enforce"} {
		p := g.Resources.Policy()
		p.Mode = mode
		g.Resources.SetPolicy(p)
		r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
		r.ContentLength = -1
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
}

func TestResourceAdmissionForwardsLargeBodyUnchanged(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 2<<20)
	g, calls := resourceGateway(t, func(w http.ResponseWriter, r *http.Request) {
		b, e := io.ReadAll(r.Body)
		if e != nil || !bytes.Equal(b, body) {
			t.Error("unlimited body changed", e)
		}
		w.WriteHeader(200)
	})
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/unlimited", bytes.NewReader(body)))
	if w.Code != 200 || calls.Load() != 1 || g.Resources.Snapshot().Rejected != 0 {
		t.Fatal("resource admission imposed a body cap", w.Code)
	}
}
