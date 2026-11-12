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
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
)

func idleTestRequest(t *testing.T, client *http.Client, address, key string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+"/v1/responses", strings.NewReader(`{"model":"test","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestIdleTimeoutAllowsSubmittedRequestToWaitForHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"status":"completed"}`)
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	g.idleTimeout = 120 * time.Millisecond
	address := timingServerForTest(t, g)
	client := &http.Client{Timeout: 3 * time.Second}
	resp := idleTestRequest(t, client, address, key)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"completed"`) {
		t.Fatalf("response=%d body=%s err=%v", resp.StatusCode, body, err)
	}
	if err := g.WaitCaptures(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _, err := st.GatewayUntracedFailures(t.Context(), time.Now().Add(-time.Minute), time.Now().Add(time.Minute), "", 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("model wait recorded as failure: %+v %v", rows, err)
	}
}

func TestIdleTimeoutStillRejectsStalledUpload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	g.idleTimeout = 120 * time.Millisecond
	address := timingServerForTest(t, g)
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 1024\r\n\r\n{", address, key)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(string(body), `"code":"idle_timeout"`) {
		t.Fatalf("stalled upload response=%d body=%s err=%v", resp.StatusCode, body, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, err := st.GatewayUntracedFailures(t.Context(), time.Now().Add(-time.Minute), time.Now().Add(time.Minute), "", 10)
		if err == nil && len(rows) == 1 {
			if rows[0].FailureReason != "idle_timeout" || rows[0].Completed || rows[0].StatusCode != 504 {
				t.Fatalf("timeout not retained: %+v", rows[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout metadata not persisted")
}

func TestIdleTimeoutAllowsSilenceInsideSubmittedStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-CPA-TRACE-ID", "test-1234abcd")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	g.idleTimeout = 120 * time.Millisecond
	address := timingServerForTest(t, g)
	resp := idleTestRequest(t, &http.Client{Timeout: 3 * time.Second}, address, key)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || err != nil || !strings.Contains(string(body), "hello") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("model silence cut off stream: status=%d body=%s err=%v", resp.StatusCode, body, err)
	}
	row := waitTiming(t, st, "1234abcd", key)
	if !row.Completed || row.FailureReason != "" || row.StatusCode != 200 {
		t.Fatalf("successful stream recorded as failure: %+v", row)
	}
}

func TestSubmittedRequestStillCancelsOnClientDisconnect(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	g, _, key := captureGatewayForTest(t, upstream.URL)
	address := timingServerForTest(t, g)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", "http://"+address+"/v1/responses", strings.NewReader(`{"model":"test","input":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+key)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, _ := http.DefaultClient.Do(r)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected client left upstream running")
	}
	<-done
}

func TestAGYSubmittedRequestAllowsQuietGeneration(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-Request-Id") != "generation-wait-identity" {
			t.Error("upstream submission tracing lost AGY retry identity")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_agy_wait","status":"completed","output":[]}`)
	}))
	defer worker.Close()
	g, st, key := captureGatewayForTest(t, worker.URL)
	g.idleTimeout = 120 * time.Millisecond
	var err error
	g.AGY, err = agy.NewClient(worker.URL, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAGYSettings(t.Context(), agy.Settings{Models: []agy.Model{{ID: "test"}}, Rules: []agy.ModelRule{{ID: "test", Enabled: true}}}, 0); err != nil {
		t.Fatal(err)
	}
	address := timingServerForTest(t, g)
	r, _ := http.NewRequestWithContext(t.Context(), "POST", "http://"+address+"/v1/responses", strings.NewReader(`{"model":"agy/test","input":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Client-Request-Id", "generation-wait-identity")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), `"status":"completed"`) {
		t.Fatalf("AGY model wait cut off: %d %s %v", resp.StatusCode, body, err)
	}
}

type pipeDeadlineWriter struct {
	*httptest.ResponseRecorder
	net.Conn
}

func (w *pipeDeadlineWriter) Write(p []byte) (int, error) { return w.Conn.Write(p) }

func TestIdleTimeoutStillAbortsBlockedClientWriteAfterSubmission(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	watch := &requestIdleWatch{timeout: 40 * time.Millisecond, last: time.Now(), cancel: cancel,
		controller: http.NewResponseController(&pipeDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), Conn: server})}
	watch.timer = time.AfterFunc(time.Hour, watch.check)
	defer watch.stop()
	watch.upstreamSubmitted()
	conn := &timingConn{Conn: server, idleWatch: watch}
	done := make(chan error, 1)
	go func() { _, err := conn.Write([]byte("blocked client")); done <- err }()
	select {
	case err := <-done:
		if err == nil || !watch.expired() || context.Cause(ctx) != errGatewayIdleTimeout {
			t.Fatalf("blocked write not timed out: %v cause=%v", err, context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("submitted request disabled stalled-client protection")
	}
}

func TestIdleTimeoutAllowsLongActiveStreamAndKeepAliveReuse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for range 12 {
			_, _ = io.WriteString(w, "data: heartbeat\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(25 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	g, _, key := captureGatewayForTest(t, upstream.URL)
	g.idleTimeout = 120 * time.Millisecond
	address := timingServerForTest(t, g)
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	for range 2 {
		resp := idleTestRequest(t, client, address, key)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "[DONE]") {
			t.Fatalf("active stream cut off: status=%d body=%s err=%v", resp.StatusCode, body, err)
		}
		time.Sleep(180 * time.Millisecond)
	}
}

func TestIdleTimeoutKeepsFailureClassificationDuringUpload(t *testing.T) {
	m := &requestMeasurement{}
	m.failure("idle_timeout", http.StatusGatewayTimeout)
	m.failure("upstream_request_failed", http.StatusBadGateway)
	w := &requestIdleWatch{timedOut: true}
	w.attachMeasurement(m)
	if m.row.FailureReason != "idle_timeout" || m.row.StatusCode != 504 {
		t.Fatal("timeout mislabeled as interrupted upload or transport failure")
	}
}
