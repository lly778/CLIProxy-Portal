package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
)

func TestAGYHandshakeAndKeepaliveDoNotShortenModelWaitTiming(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_wait\"}}\n\n")
		w.(http.Flusher).Flush()
		for range 3 {
			time.Sleep(60 * time.Millisecond)
			io.WriteString(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"real output\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_wait\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer worker.Close()
	g, st, key := captureGatewayForTest(t, worker.URL)
	g.AGY, _ = agy.NewClient(worker.URL, strings.Repeat("s", 64))
	if err := st.SaveAGYSettings(t.Context(), agy.Settings{Models: []agy.Model{{ID: "test"}}, Rules: []agy.ModelRule{{ID: "test", Enabled: true}}}, 0); err != nil {
		t.Fatal(err)
	}
	address := timingServerForTest(t, g)
	r, _ := http.NewRequestWithContext(t.Context(), "POST", "http://"+address+"/v1/responses", strings.NewReader(`{"model":"agy/test","stream":true,"input":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), ": keepalive") {
		t.Fatalf("streaming keepalive did not reach client: %d %s %v", resp.StatusCode, body, err)
	}
	if err := g.WaitCaptures(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListGatewayCaptures(t.Context(), "capture-owner", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("missing capture: %+v %v", rows, err)
	}
	row, err := st.GatewayTimingByID(t.Context(), rows[0].ID, cpamp.HashAPIKey(key))
	if err != nil || row.ResponseStartedMS == nil || row.UpstreamHeadersMS == nil || *row.ResponseStartedMS-*row.UpstreamHeadersMS < 150 || !row.Completed || row.FailureReason != "" {
		t.Fatalf("connection handshake replaced actual model timing: %+v %v", row, err)
	}
}
