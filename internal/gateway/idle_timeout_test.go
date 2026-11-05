package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestIdleTimeoutBeforeHeadersReturns504AndCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	g.idleTimeout = 120 * time.Millisecond
	address := timingServerForTest(t, g)
	client := &http.Client{Timeout: 3 * time.Second}
	resp := idleTestRequest(t, client, address, key)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(string(body), `"code":"idle_timeout"`) {
		t.Fatalf("response=%d body=%s err=%v", resp.StatusCode, body, err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("timed-out upstream request still running")
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

func TestIdleTimeoutAbortsStartedStreamWithoutSuccessfulTerminator(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-CPA-TRACE-ID", "test-1234abcd")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	g.idleTimeout = 120 * time.Millisecond
	address := timingServerForTest(t, g)
	resp := idleTestRequest(t, &http.Client{Timeout: 3 * time.Second}, address, key)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || err == nil || !strings.Contains(string(body), "hello") || strings.Contains(string(body), "[DONE]") {
		t.Fatalf("partial stream claimed completion: status=%d body=%s err=%v", resp.StatusCode, body, err)
	}
	row := waitTiming(t, st, "1234abcd", key)
	if row.Completed || row.FailureReason != "idle_timeout" || row.StatusCode != 504 {
		t.Fatalf("upstream 200 concealed downstream timeout: %+v", row)
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
