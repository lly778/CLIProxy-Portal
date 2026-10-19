package gateway

import (
	"bufio"
	"bytes"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"compress/gzip"
	"context"
	"encoding/binary"
	"github.com/klauspost/compress/zstd"
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

func TestResourceWebsocketUpgradeHoldsCapacityAndRejectsOversizedMessage(t *testing.T) {
	upClosed := make(chan struct{})
	g, _ := resourceGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-WebSocket-Extensions") != "" {
			t.Error("compression negotiated under protection")
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
	header := []byte{0x81, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4}
	binary.BigEndian.PutUint64(header[2:10], uint64(resourcecontrol.MiB+1))
	if _, e = c.Write(header); e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, reader)
	select {
	case <-upClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("oversized connection stayed open")
	}
	deadline := time.Now().Add(time.Second)
	for g.Resources.Snapshot().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	q := g.Resources.Snapshot()
	if q.Active != 0 || q.Processing != 0 || q.Rejected != 2 {
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
	p.BodyMiB = 1
	g.Resources = resourcecontrol.NewController(p)
	g.Resources.UpdateMetrics(resourcecontrol.Metrics{At: time.Now(), HostID: "test", CPUCount: 1, TotalMiB: 926, AvailableMiB: 400}, time.Now())
	return g, calls
}
func TestResourceLimitsRejectLargePlainCompressedAndChunkedBeforeCPA(t *testing.T) {
	g, calls := resourceGateway(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); w.WriteHeader(200) })
	large := bytes.Repeat([]byte("x"), (1<<20)+1)
	var gzipBody bytes.Buffer
	gz := gzip.NewWriter(&gzipBody)
	_, _ = gz.Write(large)
	_ = gz.Close()
	z, _ := zstd.NewWriter(nil)
	zbody := z.EncodeAll(large, nil)
	z.Close()
	for _, tt := range []struct {
		body     []byte
		encoding string
		chunked  bool
	}{{large, "", false}, {large, "", true}, {gzipBody.Bytes(), "gzip", false}, {zbody, "zstd", false}} {
		r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(tt.body))
		if tt.chunked {
			r.ContentLength = -1
		}
		r.Header.Set("Content-Encoding", tt.encoding)
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, r)
		if w.Code != 413 {
			t.Fatalf("%s chunked=%v: %d %s", tt.encoding, tt.chunked, w.Code, w.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("oversized request reached CPA")
	}
	q := g.Resources.Snapshot()
	if q.Active != 0 || q.Processing != 0 {
		t.Fatal("body rejection leaked capacity", q)
	}
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

type frameConnection struct {
	bytes.Buffer
	closed bool
}

func (c *frameConnection) Close() error { c.closed = true; return nil }
func maskedFrame(op byte, fin bool, size int) []byte {
	first := op
	if fin {
		first |= 128
	}
	b := []byte{first, 128 | 126, 0, 0}
	binary.BigEndian.PutUint16(b[2:4], uint16(size))
	b = append(b, 1, 2, 3, 4)
	return append(b, make([]byte, size)...)
}
func TestResourceWebsocketCountsFragmentsAndControlFrames(t *testing.T) {
	c := &frameConnection{}
	rejects := 0
	w := &resourceWebsocket{ReadWriteCloser: c, limit: 10, onReject: func() { rejects++ }}
	first := maskedFrame(1, false, 6)
	ping := []byte{137, 128, 1, 2, 3, 4}
	last := maskedFrame(0, true, 5)
	for _, b := range first {
		if _, e := w.Write([]byte{b}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := w.Write(ping); e != nil {
		t.Fatal(e)
	}
	before := c.Len()
	if _, e := w.Write(last); e == nil {
		t.Fatal("fragment limit bypassed")
	}
	if rejects != 1 || !c.closed || c.Len() != before {
		t.Fatal("oversized fragment forwarded")
	}
	c = &frameConnection{}
	w = &resourceWebsocket{ReadWriteCloser: c, limit: 10, onReject: func() { t.Error("valid frame rejected") }}
	valid := maskedFrame(1, true, 5)
	if _, e := w.Write(append(valid, valid...)); e != nil {
		t.Fatal(e)
	}
}

func TestResourceZeroBodyLimitForwardsLargeBodyUnchanged(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 2<<20)
	g, calls := resourceGateway(t, func(w http.ResponseWriter, r *http.Request) {
		b, e := io.ReadAll(r.Body)
		if e != nil || !bytes.Equal(b, body) {
			t.Error("unlimited body changed", e)
		}
		w.WriteHeader(200)
	})
	p := g.Resources.Policy()
	p.BodyMiB = 0
	g.Resources.SetPolicy(p)
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/unlimited", bytes.NewReader(body)))
	if w.Code != 200 || calls.Load() != 1 || g.Resources.Snapshot().Rejected != 0 {
		t.Fatal("zero body cap rejected", w.Code)
	}
}
