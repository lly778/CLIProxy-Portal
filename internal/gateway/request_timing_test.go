package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

func timingServerForTest(t *testing.T, g *Gateway) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: g.Handler()}
	ConfigureServerTiming(s)
	go func() { _ = s.Serve(TimingListener(l)) }()
	t.Cleanup(func() { _ = s.Close() })
	return l.Addr().String()
}

func waitTiming(t *testing.T, st *store.Store, id, key string) store.GatewayRequestTiming {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := st.GatewayTimingByCPARequest(context.Background(), id, cpamp.HashAPIKey(key))
		if err == nil {
			return row
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timing was not persisted")
	return store.GatewayRequestTiming{}
}

func readTimingResponse(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTotalTimingIncludesHeadersUploadAndStreamingButExcludesKeepAliveIdle(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		id := fmt.Sprintf("%08x", calls.Add(1))
		w.Header().Set("X-CPA-TRACE-ID", "test-"+id)
		w.Header().Set("Content-Type", "text/event-stream")
		if id == "00000001" {
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	address := timingServerForTest(t, g)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	body := `{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}`
	_, _ = fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: example\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n", key, len(body))
	// Delay the final header terminator, then delay the rest of the body.
	time.Sleep(100 * time.Millisecond)
	_, _ = io.WriteString(conn, "\r\n"+body[:10])
	time.Sleep(150 * time.Millisecond)
	_, _ = io.WriteString(conn, body[10:])
	reader := bufio.NewReader(conn)
	readTimingResponse(t, reader)
	first := waitTiming(t, st, "00000001", key)
	if first.StartSource != "connection_accepted" || !first.Completed || first.RequestReadMS == nil || *first.RequestReadMS < 230 || first.TotalMS < 380 {
		t.Fatalf("upload/header/generation delays missing: %+v", first)
	}
	if first.UpstreamHeadersMS == nil || first.ResponseStartedMS == nil || *first.UpstreamHeadersMS < *first.RequestReadMS || *first.ResponseStartedMS < *first.UpstreamHeadersMS || first.TotalMS-*first.ResponseStartedMS < 90 {
		t.Fatalf("stream stages are inconsistent: %+v", first)
	}

	// Same connection, with substantial idle time: it must not inflate the
	// next request. Also exercise EOF detection for chunked request bodies.
	time.Sleep(600 * time.Millisecond)
	_, _ = fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: example\r\nAuthorization: Bearer %s\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", key, len(body), body)
	readTimingResponse(t, reader)
	second := waitTiming(t, st, "00000002", key)
	if second.StartSource != "request_received" || second.TotalMS >= 300 || !second.Completed || second.RequestReadMS == nil || second.ResponseStartedMS == nil {
		t.Fatalf("keep-alive idle included or chunked EOF/final flush missing: %+v", second)
	}
}

func TestTimingPersistsWithoutDialogueAndAfterFinalBufferedWrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-CPA-TRACE-ID", "test-abc12345")
		// Small response remains in net/http's buffer until after ServeHTTP.
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	address := timingServerForTest(t, g)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: example\r\nAuthorization: Bearer %s\r\nConnection: close\r\nContent-Length: 2\r\n\r\n{}", key)
	readTimingResponse(t, bufio.NewReader(conn))
	row := waitTiming(t, st, "abc12345", key)
	if !row.Completed || row.ResponseStartedMS == nil || row.TotalMS < *row.ResponseStartedMS || row.EndedAt.Before(row.StartedAt) {
		t.Fatalf("final buffered response write missing: %+v", row)
	}
	rows, _ := st.ListGatewayCaptures(context.Background(), "capture-owner", 100)
	if len(rows) != 0 {
		t.Fatalf("empty request/response unexpectedly had dialogue: %+v", rows)
	}
}

func TestCancelledStreamTimingIsMarkedIncomplete(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-CPA-TRACE-ID", "test-dead1234")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)
	g, st, key := captureGatewayForTest(t, upstream.URL)
	address := timingServerForTest(t, g)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	body := `{"model":"test","input":"hello","stream":true}`
	_, _ = fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: example\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", key, len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "first") {
		t.Fatalf("stream did not start: %q %v", line, err)
	}
	_ = conn.Close()
	row := waitTiming(t, st, "dead1234", key)
	if row.Completed || row.ResponseStartedMS == nil {
		t.Fatalf("cancelled stream timing = %+v", row)
	}
}

func TestInterruptedUploadIsLoggedWithoutCallingUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	if err := st.SetCodexIdentityCompatibility(t.Context(), "antigravity", true); err != nil {
		t.Fatal(err)
	}
	address := timingServerForTest(t, g)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now().Add(-time.Second)
	_, _ = fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: example\r\nAuthorization: Bearer %s\r\nContent-Length: 200\r\n\r\n{\"model\":", key)
	time.Sleep(80 * time.Millisecond)
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("error status %d", resp.StatusCode)
	}
	deadline := time.Now().Add(4 * time.Second)
	var row store.GatewayRequestTiming
	for time.Now().Before(deadline) {
		rows, _, err := st.GatewayUntracedFailures(t.Context(), start, time.Now().Add(time.Second), "capture-owner", 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			row = rows[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if row.ID == "" || row.FailureReason != "upload_interrupted" || row.StatusCode != 502 || row.Completed || row.RequestReadMS != nil || row.CPARequestID != "" || row.ResponseStartedMS == nil || row.TotalMS < 70 || calls.Load() != 0 {
		t.Fatalf("interrupted upload lost/misclassified: %+v, upstream calls %d", row, calls.Load())
	}
	totals, err := st.GatewayTimingTotals(t.Context(), start, time.Now().Add(time.Second), "capture-owner", start, time.Hour)
	if err != nil || len(totals) != 1 || totals[0].StageSamples != 1 || totals[0].WaitMS != 0 || totals[0].UploadMS+totals[0].ResponseMS != row.TotalMS {
		t.Fatalf("interrupted stages: %+v %v", totals, err)
	}
}
